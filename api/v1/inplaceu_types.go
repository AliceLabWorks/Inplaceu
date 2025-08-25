/*
Copyright 2025.

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

package v1

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// CloneSetInstanceID is a unique id for Pods and PVCs.
	// Each pod and the pvcs it owns have the same instance-id.
	InplaceuInstanceID = "apps.kruise.io/cloneset-instance-id"

	SpecifiedDeleteKey = "apps.kruise.io/specified-delete"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// InplaceuSpec defines the desired state of Inplaceu.
type InplaceuSpec struct {
	// Number of desired pods. This is a pointer to distinguish between explicit
	// zero and not specified. Defaults to 1.
	Replicas *int32 `json:"replicas,omitempty"`

	// Label selector for pods. Existing ReplicaSets whose pods are
	// selected by this will be the ones affected by this deployment.
	// It must match the pod template's labels.
	Selector *metav1.LabelSelector `json:"selector"`

	// Template describes the pods that will be created.
	// The only allowed template.spec.restartPolicy value is "Always".
	// +kubebuilder:pruning:PreserveUnknownFields
	Template corev1.PodTemplateSpec `json:"template"`

	// The deployment strategy to use to replace existing pods with new ones.
	Strategy appsv1.DeploymentStrategy `json:"strategy,omitempty"`

	// Minimum number of seconds for which a newly created pod should be ready
	// without any of its container crashing, for it to be considered available.
	// Defaults to 0 (pod will be considered available as soon as it is ready)
	MinReadySeconds int32 `json:"minReadySeconds,omitempty"`

	// The number of old ReplicaSets to retain to allow rollback.
	// This is a pointer to distinguish between explicit zero and not specified.
	// Defaults to 10.
	RevisionHistoryLimit *int32 `json:"revisionHistoryLimit,omitempty"`

	// Indicates that the deployment is paused.
	Paused bool `json:"paused,omitempty"`

	// The maximum time in seconds for a deployment to make progress before it
	// is considered to be failed. The deployment controller will continue to
	// process failed deployments and a condition with a ProgressDeadlineExceeded
	// reason will be surfaced in the deployment status. Note that progress will
	// not be estimated during the time a deployment is paused. Defaults to 600s.
	ProgressDeadlineSeconds *int32 `json:"progressDeadlineSeconds,omitempty"`
}

// InplaceuStatus defines the observed state of Inplaceu.
type InplaceuStatus struct {
	// The generation observed by the deployment controller.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Total number of non-terminating pods targeted by this deployment (their labels match the selector).
	Replicas int32 `json:"replicas,omitempty"`
	// Total number of non-terminating pods targeted by this deployment that have the desired template spec.
	UpdatedReplicas int32 `json:"updatedReplicas,omitempty"`
	// Total number of non-terminating pods targeted by this Deployment with a Ready Condition.
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`
	// Total number of available non-terminating pods (ready for at least minReadySeconds) targeted by this deployment.
	AvailableReplicas int32 `json:"availableReplicas,omitempty"`
	// Total number of unavailable pods targeted by this deployment. This is the total number of
	// pods that are still required for the deployment to have 100% available capacity. They may
	// either be pods that are running but not yet available or pods that still have not been created.
	UnavailableReplicas int32 `json:"unavailableReplicas,omitempty"`
	// Total number of terminating pods targeted by this deployment. Terminating pods have a non-null
	// .metadata.deletionTimestamp and have not yet reached the Failed or Succeeded .status.phase.
	//
	// This is an alpha field. Enable DeploymentReplicaSetTerminatingReplicas to be able to use this field.
	TerminatingReplicas *int32 `json:"terminatingReplicas,omitempty"`
	// Represents the latest available observations of a deployment's current state.
	Conditions []appsv1.DeploymentCondition `json:"conditions,omitempty"`

	// Count of hash collisions for the Deployment. The Deployment controller uses this
	// field as a collision avoidance mechanism when it needs to create the name for the
	// newest ReplicaSet.
	CollisionCount *int32 `json:"collisionCount,omitempty"`

	// UpdateRevision, if not empty, indicates the latest revision of the CloneSet.
	UpdateRevision string `json:"updateRevision,omitempty"`

	// currentRevision, if not empty, indicates the current revision version of the CloneSet.
	CurrentRevision string `json:"currentRevision,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// Inplaceu is the Schema for the inplaceus API.
type Inplaceu struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   InplaceuSpec   `json:"spec,omitempty"`
	Status InplaceuStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// InplaceuList contains a list of Inplaceu.
type InplaceuList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Inplaceu `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Inplaceu{}, &InplaceuList{})
}
