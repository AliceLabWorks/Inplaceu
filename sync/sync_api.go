/*
Copyright 2021 The Kruise Authors.

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

package sync

import (
	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batchv1 "inplace.kubebuilder.io/project/api/v1"
	inplaceupdate "inplace.kubebuilder.io/project/inplace_update"
	inplaceutils "inplace.kubebuilder.io/project/utils"
)

// Interface for managing pods scaling and updating.
type Interface interface {
	Scale(
		currentCS, updateCS *batchv1.Inplaceu,
		currentRevision, updateRevision string,
		pods []*corev1.Pod,
	) (bool, error)

	Update(cs *batchv1.Inplaceu,
		currentRevision, updateRevision *apps.ControllerRevision, revisions []*apps.ControllerRevision,
		pods []*corev1.Pod,
	) error
}

type realControl struct {
	client.Client
	recorder       record.EventRecorder
	inplaceControl inplaceupdate.Interface
}

func New(c client.Client, recorder record.EventRecorder) Interface {
	return &realControl{
		Client:         c,
		inplaceControl: inplaceupdate.New(c, inplaceutils.RevisionAdapterImpl),
		recorder:       recorder,
	}
}
