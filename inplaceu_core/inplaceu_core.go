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
	"encoding/json"
	"fmt"
	"regexp"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubecontroller "k8s.io/kubernetes/pkg/controller"

	batchv1 "inplace.kubebuilder.io/project/api/v1"
	v1 "inplace.kubebuilder.io/project/api/v1"
	utils "inplace.kubebuilder.io/project/utils"
)

var (
	inPlaceUpdateTemplateSpecPatchRexp = regexp.MustCompile("^/containers/([0-9]+)/image$")
)

type commonControl struct {
	*batchv1.Inplaceu
}

var _ Control = &commonControl{}

func (c *commonControl) IsInitializing() bool {
	return false
}

func (c *commonControl) SetRevisionTemplate(revisionSpec map[string]interface{}, template map[string]interface{}) {
	revisionSpec["template"] = template
	template["$patch"] = "replace"
}

func (c *commonControl) ApplyRevisionPatch(patched []byte) (*batchv1.Inplaceu, error) {
	restoredSet := &batchv1.Inplaceu{}
	if err := json.Unmarshal(patched, restoredSet); err != nil {
		return nil, err
	}
	return restoredSet, nil
}

func (c *commonControl) IsReadyToScale() bool {
	return true
}

func (c *commonControl) NewVersionedPods(updateCS *batchv1.Inplaceu,
	updateRevision string,
	expectedCreations int,
	availableIDs []string,
) ([]*corev1.Pod, error) {
	var newPods []*corev1.Pod
	newPods = append(newPods, c.newVersionedPods(updateCS, updateRevision, expectedCreations, &availableIDs)...)
	return newPods, nil
}

func (c *commonControl) newVersionedPods(iu *batchv1.Inplaceu, revision string, replicas int, availableIDs *[]string) []*corev1.Pod {
	var newPods []*corev1.Pod
	for i := 0; i < replicas; i++ {
		if len(*availableIDs) == 0 {
			return newPods
		}
		id := (*availableIDs)[0]
		*availableIDs = (*availableIDs)[1:]

		pod, _ := kubecontroller.GetPodFromTemplate(&iu.Spec.Template, iu, metav1.NewControllerRef(iu, utils.ControllerKind))
		if pod.Labels == nil {
			pod.Labels = make(map[string]string)
		}
		utils.WriteRevisionHash(pod, revision)

		pod.Name = fmt.Sprintf("%s-%s", iu.Name, id)
		pod.Namespace = iu.Namespace
		pod.Labels[v1.InplaceuInstanceID] = id

		newPods = append(newPods, pod)
	}
	return newPods
}

func (c *commonControl) IsPodUpdatePaused(pod *corev1.Pod) bool {
	return false
}

func (c *commonControl) GetPodsSortFunc(pods []*corev1.Pod, waitUpdateIndexes []int) func(i, j int) bool {
	// not-ready < ready, unscheduled < scheduled, and pending < running
	return func(i, j int) bool {
		return kubecontroller.ActivePods(pods).Less(waitUpdateIndexes[i], waitUpdateIndexes[j])
	}
}
