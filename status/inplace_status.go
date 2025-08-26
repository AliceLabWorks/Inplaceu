/*
Copyright 2019 The Kruise Authors.

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

package inplaceu

import (
	"context"

	batchv1 "inplace.kubebuilder.io/project/api/v1"
	inplaceutils "inplace.kubebuilder.io/project/utils"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// StatusUpdater is interface for updating Inplaceu status.
type StatusUpdater interface {
	UpdateInplaceuSetStatus(iu *batchv1.Inplaceu, newStatus *batchv1.InplaceuStatus, pods []*v1.Pod) error
}

func NewStatusUpdater(c client.Client) StatusUpdater {
	return &realStatusUpdater{Client: c}
}

type realStatusUpdater struct {
	client.Client
}

func (r *realStatusUpdater) UpdateInplaceuSetStatus(iu *batchv1.Inplaceu, newStatus *batchv1.InplaceuStatus, pods []*v1.Pod) error {
	r.calculateStatus(iu, newStatus, pods)
	if !r.inconsistentStatus(iu, newStatus) {
		return nil
	}
	klog.InfoS("To update inplaceU status", "inplaceU", klog.KObj(iu), "replicas", newStatus.Replicas, "ready", newStatus.ReadyReplicas, "available", newStatus.AvailableReplicas,
		"updated", newStatus.UpdatedReplicas, "currentRevision", newStatus.CurrentRevision, "updateRevision", newStatus.UpdateRevision)
	return r.updateStatus(iu, newStatus)
}

func (r *realStatusUpdater) updateStatus(iu *batchv1.Inplaceu, newStatus *batchv1.InplaceuStatus) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		clone := &batchv1.Inplaceu{}
		if err := r.Get(context.TODO(), types.NamespacedName{Namespace: iu.Namespace, Name: iu.Name}, clone); err != nil {
			return err
		}
		clone.Status = *newStatus
		return r.Status().Update(context.TODO(), clone)
	})
}

func (r *realStatusUpdater) inconsistentStatus(iu *batchv1.Inplaceu, newStatus *batchv1.InplaceuStatus) bool {
	oldStatus := iu.Status
	return newStatus.ObservedGeneration > oldStatus.ObservedGeneration ||
		newStatus.Replicas != oldStatus.Replicas ||
		newStatus.ReadyReplicas != oldStatus.ReadyReplicas ||
		newStatus.AvailableReplicas != oldStatus.AvailableReplicas ||
		newStatus.UpdatedReplicas != oldStatus.UpdatedReplicas ||
		newStatus.UpdateRevision != oldStatus.UpdateRevision ||
		newStatus.CurrentRevision != oldStatus.CurrentRevision
}

func (r *realStatusUpdater) calculateStatus(iu *batchv1.Inplaceu, newStatus *batchv1.InplaceuStatus, pods []*v1.Pod) {
	for _, pod := range pods {
		newStatus.Replicas++
		if inplaceutils.IsRunning(pod) {
			newStatus.ReadyReplicas++
		}
		if inplaceutils.IsPodAvailable(pod, iu.Spec.MinReadySeconds) {
			newStatus.AvailableReplicas++
		}
		if inplaceutils.EqualToRevisionHash("", pod, newStatus.UpdateRevision) {
			newStatus.UpdatedReplicas++
		}
	}
	// Consider the update revision as stable if revisions of all pods are consistent to it and have the expected number of replicas, no need to wait all of them ready
	if newStatus.UpdatedReplicas == newStatus.Replicas && newStatus.Replicas == *iu.Spec.Replicas {
		newStatus.CurrentRevision = newStatus.UpdateRevision
	}
}
