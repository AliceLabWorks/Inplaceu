/*
Copyright 2020 The Kruise Authors.

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

package inplacecore

import (
	v1 "k8s.io/api/core/v1"

	batchv1 "inplace.kubebuilder.io/project/api/v1"
)

type Control interface {
	// common
	IsInitializing() bool
	SetRevisionTemplate(revisionSpec map[string]interface{}, template map[string]interface{})
	ApplyRevisionPatch(patched []byte) (*batchv1.Inplaceu, error)

	// scale
	IsReadyToScale() bool
	NewVersionedPods(updateCS *batchv1.Inplaceu,
		updateRevision string,
		expectedCreations int,
		availableIDs []string,
	) ([]*v1.Pod, error)

	// update
	GetPodsSortFunc(pods []*v1.Pod, waitUpdateIndexes []int) func(i, j int) bool
}

func New(cs *batchv1.Inplaceu) Control {
	return &commonControl{Inplaceu: cs}
}
