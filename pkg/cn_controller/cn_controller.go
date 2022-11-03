/*
Copyright 2021-present, StarRocks Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cn_controller

import (
	"context"
	"errors"
	srapi "github.com/StarRocks/starrocks-kubernetes-operator/api/v1alpha1"
	rutils "github.com/StarRocks/starrocks-kubernetes-operator/pkg/common/resource_utils"
	"github.com/StarRocks/starrocks-kubernetes-operator/pkg/k8sutils"
	appv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"strconv"
)

type CnController struct {
	k8sclient   client.Client
	k8srecorder record.EventRecorder
}

func New(k8sclient client.Client, k8srecorder record.EventRecorder) *CnController {
	return &CnController{
		k8sclient:   k8sclient,
		k8srecorder: k8srecorder,
	}
}

func (cc *CnController) Sync(ctx context.Context, src *srapi.StarRocksCluster) error {
	if src.Spec.StarRocksCnSpec == nil {
		klog.Info("CnController Sync ", "the cn component is not needed", " namespace ", src.Namespace, " starrocks cluster name ", src.Name)
		return nil
	}

	cnSpec := src.Spec.StarRocksCnSpec
	cs := &srapi.StarRocksCnStatus{}
	cs.Phase = srapi.ComponentWaiting
	src.Status.StarRocksCnStatus = cs
	endpoints := corev1.Endpoints{}
	//1. wait for fe ok.
	if err := cc.k8sclient.Get(ctx, types.NamespacedName{Namespace: src.Namespace, Name: srapi.GetFeExternalServiceName(src)}, &endpoints); apierrors.IsNotFound(err) || len(endpoints.Subsets) == 0 {
		klog.Info("wait fe available fe service name ", srapi.GetFeExternalServiceName(src))
		return nil
	} else if err != nil {
		return err
	}

	feReady := false
	for _, sub := range endpoints.Subsets {
		if len(sub.Addresses) > 0 {
			feReady = true
			break
		}
	}
	if !feReady {
		klog.Info("wait fe available fe service name ", srapi.GetFeExternalServiceName(src), " have not ready fe.")
		return nil
	}

	//get the cn configMap for resolve ports.
	//2. get config for generate statefulset and service.
	config, err := cc.GetCnConfig(ctx, &cnSpec.ConfigMapInfo, src.Namespace)
	if err != nil {
		klog.Error("CnController Sync ", "resolve cn configmap failed, namespace ", src.Namespace, " configmapName ", cnSpec.ConfigMapInfo.ConfigMapName, " configMapKey ", cnSpec.ConfigMapInfo.ResolveKey, " error ", err)
		return err
	}

	feconfig, _ := cc.getFeConfig(ctx, &src.Spec.StarRocksFeSpec.ConfigMapInfo, src.Namespace)
	//annotation: add query port in cnconfig.
	config[rutils.QUERY_PORT] = strconv.FormatInt(int64(rutils.GetPort(feconfig, rutils.QUERY_PORT)), 10)

	//generate new cn internal service.
	externalsvc := rutils.BuildExternalService(src, srapi.GetCnExternalServiceName(src), rutils.CnService, config)
	insvc := &corev1.Service{}
	externalsvc.ObjectMeta.DeepCopyInto(&insvc.ObjectMeta)
	insvc.Name = cc.getCnDomainService()
	insvc.Spec = corev1.ServiceSpec{
		Ports: []corev1.ServicePort{
			{
				Name:       "heartbeat",
				Port:       rutils.GetPort(config, rutils.HEARTBEAT_SERVICE_PORT),
				TargetPort: intstr.FromInt(int(rutils.GetPort(config, rutils.HEARTBEAT_SERVICE_PORT))),
			},
		},
		Selector: externalsvc.Spec.Selector,

		//value = true, Pod don't need to become ready that be search by domain.
		PublishNotReadyAddresses: true,
	}
	cs.ServiceName = insvc.Name
	//create or update fe service, update the status of cn on src.
	//3. issue the service.
	if err := k8sutils.CreateOrUpdateService(ctx, cc.k8sclient, insvc); err != nil {
		klog.Error("CnController Sync ", "create or update service namespace ", insvc.Namespace, " name ", insvc.Name)
		return err
	}

	cnFinalizers := []string{srapi.CN_SERVICE_FINALIZER}
	//4. create cn statefulset.
	st := rutils.NewStatefulset(cc.buildStatefulSetParams(src, config))
	st.Spec.PodManagementPolicy = appv1.ParallelPodManagement
	defer func() {
		src.Finalizers = rutils.MergeSlices(src.Finalizers, cnFinalizers)
		cs.ResourceNames = rutils.MergeSlices(cs.ResourceNames, []string{st.Name})
	}()

	var cst appv1.StatefulSet
	err = cc.k8sclient.Get(ctx, types.NamespacedName{Namespace: st.Namespace, Name: st.Name}, &cst)
	if err != nil && apierrors.IsNotFound(err) {
		return k8sutils.CreateClientObject(ctx, cc.k8sclient, &st)
	} else if err != nil {
		return err
	}

	//5. last update the status.
	//if the spec is changed, update the status of cn on src.
	if !rutils.StatefulSetDeepEqual(&st, cst) {
		klog.Info("cnController Sync exist statefulset not equals to new statefuslet")
		rutils.MergeStatefulSets(&st, cst)
		if err := k8sutils.UpdateClientObject(ctx, cc.k8sclient, &st); err != nil {
			return err
		}
	}

	//no changed update the status of cn on src.
	return cc.updateCnStatus(cs, cst)
}

func (cc *CnController) ClearResources(ctx context.Context, src *srapi.StarRocksCluster) (bool, error) {
	//if the starrocks is not have cn.
	if src.Status.StarRocksCnStatus == nil {
		return true, nil
	}

	fmap := map[string]bool{}
	count := 0
	defer func() {
		var finalizers []string
		for _, f := range src.Finalizers {
			if _, ok := fmap[f]; !ok {
				finalizers = append(finalizers, f)
			}
		}
		src.Finalizers = finalizers
	}()

	for _, name := range src.Status.StarRocksCnStatus.ResourceNames {
		if _, err := k8sutils.DeleteClientObject(ctx, cc.k8sclient, src.Namespace, name); err != nil {
			return false, errors.New("cn delete statefulset" + err.Error())
		}
	}

	if count == len(src.Status.StarRocksCnStatus.ResourceNames) {
		fmap[srapi.CN_STATEFULSET_FINALIZER] = true
	}

	if _, ok := fmap[srapi.CN_STATEFULSET_FINALIZER]; !ok {
		return k8sutils.DeleteClientObject(ctx, cc.k8sclient, src.Namespace, src.Status.StarRocksCnStatus.ServiceName)
	}

	return false, nil
}

//updateCnStatus update the src status about cn status.
func (cc *CnController) updateCnStatus(cs *srapi.StarRocksCnStatus, st appv1.StatefulSet) error {
	var podList corev1.PodList
	if err := cc.k8sclient.List(context.Background(), &podList, client.InNamespace(st.Namespace), client.MatchingLabels(st.Spec.Selector.MatchLabels)); err != nil {
		return err
	}

	var creatings, readys, faileds []string
	podmap := make(map[string]corev1.Pod)
	//get all pod status that controlled by st.
	for _, pod := range podList.Items {
		//TODO: test
		podmap[pod.Name] = pod
		if ready := k8sutils.PodIsReady(&pod.Status); ready {
			readys = append(readys, pod.Name)
		} else if pod.Status.Phase == corev1.PodPending {
			creatings = append(creatings, pod.Name)
		} else if pod.Status.Phase == corev1.PodFailed {
			faileds = append(faileds, pod.Name)
		}
	}

	cs.Phase = srapi.ComponentReconciling
	if len(readys) == int(*st.Spec.Replicas) {
		cs.Phase = srapi.ComponentRunning
	} else if len(faileds) != 0 {
		cs.Phase = srapi.ComponentFailed
		cs.Reason = podmap[faileds[0]].Status.Reason
	} else if len(creatings) != 0 {
		cs.Phase = srapi.ComponentReconciling
		cs.Reason = podmap[creatings[0]].Status.Reason
	}

	return nil
}

func (cc *CnController) GetCnConfig(ctx context.Context, configMapInfo *srapi.ConfigMapInfo, namespace string) (map[string]interface{}, error) {
	configMap, err := k8sutils.GetConfigMap(ctx, cc.k8sclient, namespace, configMapInfo.ConfigMapName)
	if err != nil && apierrors.IsNotFound(err) {
		klog.Info("the CnController get cn config is not exist namespace ", namespace, " configmapName ", configMapInfo.ConfigMapName)
		return make(map[string]interface{}), nil
	} else if err != nil {
		return make(map[string]interface{}), err
	}

	res, err := rutils.ResolveConfigMap(configMap, configMapInfo.ResolveKey)
	return res, err
}

func (cc *CnController) getFeConfig(ctx context.Context, feconfigMapInfo *srapi.ConfigMapInfo, namespace string) (map[string]interface{}, error) {

	feconfigMap, err := k8sutils.GetConfigMap(ctx, cc.k8sclient, namespace, feconfigMapInfo.ConfigMapName)
	if err != nil && apierrors.IsNotFound(err) {
		klog.Info("the CnController get fe config is not exist namespace ", namespace, " configmapName ", feconfigMapInfo.ConfigMapName)
		return make(map[string]interface{}), nil
	} else if err != nil {
		return make(map[string]interface{}), err
	}
	res, err := rutils.ResolveConfigMap(feconfigMap, feconfigMapInfo.ResolveKey)
	return res, err
}

//getCnDomainService get the cn service name for dns resolve.
func (cc *CnController) getCnDomainService() string {
	return "cn-domain-search"
}