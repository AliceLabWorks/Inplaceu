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

package inplaceupdate

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gomodules.xyz/jsonpatch/v2"

	inplaceutils "inplace.kubebuilder.io/project/internal/utils"
	"inplace.kubebuilder.io/project/internal/utils/podadapter"
	revisionadapter "inplace.kubebuilder.io/project/internal/utils/revision_adapter"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"

	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	client "sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// InPlaceUpdateStateKey records the state of inplace-update.
	// The value of annotation is InPlaceUpdateState.
	InPlaceUpdateStateKey string = "batch.inplace.kubebuilder.io/inplace-update-state"
)

var (
	containerImagePatchRexp             = regexp.MustCompile("^/spec/containers/([0-9]+)/image$")
	Clock                   clock.Clock = clock.RealClock{}
)

type RefreshResult struct {
	RefreshErr    error
	DelayDuration time.Duration
}

type UpdateResult struct {
	InPlaceUpdate      bool
	UpdateErr          error
	DelayDuration      time.Duration
	NewResourceVersion string
}

type InPlaceUpdateContainerStatus struct {
	ImageID string `json:"imageID,omitempty"`
}

type InPlaceUpdateState struct {
	// Revision is the updated revision hash.
	Revision string `json:"revision"`

	// UpdateTimestamp is the start time when the in-place update happens.
	UpdateTimestamp metav1.Time `json:"updateTimestamp"`

	// LastContainerStatuses records the before-in-place-update container statuses. It is a map from ContainerName
	// to InPlaceUpdateContainerStatus
	LastContainerStatuses map[string]InPlaceUpdateContainerStatus `json:"lastContainerStatuses"`

	// UpdateEnvFromMetadata indicates there are envs from annotations/labels that should be in-place update.
	UpdateEnvFromMetadata bool `json:"updateEnvFromMetadata,omitempty"`

	// UpdateResources indicates there are resources that should be in-place update.
	UpdateResources bool `json:"updateResources,omitempty"`

	// UpdateImages indicates there are images that should be in-place update.
	UpdateImages bool `json:"updateImages,omitempty"`

	// NextContainerImages is the containers with lower priority that waiting for in-place update images in next batch.
	NextContainerImages map[string]string `json:"nextContainerImages,omitempty"`

	// NextContainerRefMetadata is the containers with lower priority that waiting for in-place update labels/annotations in next batch.
	NextContainerRefMetadata map[string]metav1.ObjectMeta `json:"nextContainerRefMetadata,omitempty"`

	// NextContainerResources is the containers with lower priority that waiting for in-place update resources in next batch.
	NextContainerResources map[string]corev1.ResourceRequirements `json:"nextContainerResources,omitempty"`

	// PreCheckBeforeNext is the pre-check that must pass before the next containers can be in-place update.
	PreCheckBeforeNext *InPlaceUpdatePreCheckBeforeNext `json:"preCheckBeforeNext,omitempty"`

	// ContainerBatchesRecord records the update batches that have patched in this revision.
	ContainerBatchesRecord []InPlaceUpdateContainerBatch `json:"containerBatchesRecord,omitempty"`
}

// InPlaceUpdatePreCheckBeforeNext contains the pre-check that must pass before the next containers can be in-place update.
type InPlaceUpdatePreCheckBeforeNext struct {
	ContainersRequiredReady []string `json:"containersRequiredReady,omitempty"`
}

// InPlaceUpdateContainerBatch indicates the timestamp and containers for a batch update
type InPlaceUpdateContainerBatch struct {
	// Timestamp is the time for this update batch
	Timestamp metav1.Time `json:"timestamp"`
	// Containers is the name list of containers for this update batch
	Containers []string `json:"containers"`
}

type Interface interface {
	CanUpdateInPlace(oldRevision, newRevision *apps.ControllerRevision) bool
	Update(pod *corev1.Pod, oldRevision, newRevision *apps.ControllerRevision) UpdateResult
}

type UpdateSpec struct {
	Revision string `json:"revision"`

	ContainerImages       map[string]string                      `json:"containerImages,omitempty"`
	ContainerRefMetadata  map[string]metav1.ObjectMeta           `json:"containerRefMetadata,omitempty"`
	ContainerResources    map[string]corev1.ResourceRequirements `json:"containerResources,omitempty"`
	MetaDataPatch         []byte                                 `json:"metaDataPatch,omitempty"`
	UpdateEnvFromMetadata bool                                   `json:"updateEnvFromMetadata,omitempty"`

	OldTemplate *corev1.PodTemplateSpec `json:"oldTemplate,omitempty"`
	NewTemplate *corev1.PodTemplateSpec `json:"newTemplate,omitempty"`
}

type realControl struct {
	podAdapter      podadapter.Adapter
	revisionAdapter revisionadapter.Interface
}

func New(c client.Client, revisionAdapter revisionadapter.Interface) Interface {
	return &realControl{podAdapter: &podadapter.AdapterRuntimeClient{Client: c}, revisionAdapter: revisionAdapter}
}

func NewForTypedClient(c clientset.Interface, revisionAdapter revisionadapter.Interface) Interface {
	return &realControl{podAdapter: &podadapter.AdapterTypedClient{Client: c}, revisionAdapter: revisionAdapter}
}

func NewForInformer(informer coreinformers.PodInformer, revisionAdapter revisionadapter.Interface) Interface {
	return &realControl{podAdapter: &podadapter.AdapterInformer{PodInformer: informer}, revisionAdapter: revisionAdapter}
}

func (c *realControl) CanUpdateInPlace(oldRevision, newRevision *apps.ControllerRevision) bool {
	return CalculateSpec(oldRevision, newRevision) != nil
}

func CalculateSpec(oldRevision, newRevision *apps.ControllerRevision) *UpdateSpec {
	if oldRevision == nil || newRevision == nil {
		return nil
	}

	patches, err := jsonpatch.CreatePatch(oldRevision.Data.Raw, newRevision.Data.Raw)
	if err != nil {
		return nil
	}

	oldTemp, err := GetTemplateFromRevision(oldRevision)
	if err != nil {
		return nil
	}
	newTemp, err := GetTemplateFromRevision(newRevision)
	if err != nil {
		return nil
	}

	updateSpec := &UpdateSpec{
		Revision:             newRevision.Name,
		ContainerImages:      make(map[string]string),
		ContainerResources:   make(map[string]corev1.ResourceRequirements),
		ContainerRefMetadata: make(map[string]metav1.ObjectMeta),
	}

	// all patches for podSpec can just update images in pod spec
	var metadataPatches []jsonpatch.Operation
	for _, op := range patches {
		op.Path = strings.Replace(op.Path, "/spec/template", "", 1)

		if !strings.HasPrefix(op.Path, "/spec/") {
			if strings.HasPrefix(op.Path, "/metadata/") {
				metadataPatches = append(metadataPatches, op)
				continue
			}
			return nil
		}

		if op.Operation != "replace" {
			return nil
		}
		if containerImagePatchRexp.MatchString(op.Path) {
			// for example: /spec/containers/0/image
			words := strings.Split(op.Path, "/")
			idx, _ := strconv.Atoi(words[3])
			if len(oldTemp.Spec.Containers) <= idx {
				return nil
			}
			updateSpec.ContainerImages[oldTemp.Spec.Containers[idx].Name] = op.Value.(string)
			continue
		}
		return nil
	}

	if len(metadataPatches) > 0 {
		oldBytes, _ := json.Marshal(corev1.Pod{ObjectMeta: oldTemp.ObjectMeta})
		newBytes, _ := json.Marshal(corev1.Pod{ObjectMeta: newTemp.ObjectMeta})
		patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldBytes, newBytes, &corev1.Pod{})
		if err != nil {
			return nil
		}
		updateSpec.MetaDataPatch = patchBytes
	}

	return updateSpec
}

// GetTemplateFromRevision returns the pod template parsed from ControllerRevision.
func GetTemplateFromRevision(revision *apps.ControllerRevision) (*corev1.PodTemplateSpec, error) {
	var patchObj *struct {
		Spec struct {
			Template corev1.PodTemplateSpec `json:"template"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(revision.Data.Raw, &patchObj); err != nil {
		return nil, err
	}
	return &patchObj.Spec.Template, nil
}

func (c *realControl) Update(pod *corev1.Pod, oldRevision, newRevision *apps.ControllerRevision) UpdateResult {
	// 1. calculate inplace update spec
	spec := CalculateSpec(oldRevision, newRevision)
	if spec == nil {
		return UpdateResult{}
	}

	// 2. update container images
	newResourceVersion, err := c.updatePodInPlace(pod, spec)
	if err != nil {
		return UpdateResult{InPlaceUpdate: true, UpdateErr: err}
	}

	var delayDuration time.Duration
	return UpdateResult{InPlaceUpdate: true, DelayDuration: delayDuration, NewResourceVersion: newResourceVersion}
}

func (c *realControl) updatePodInPlace(pod *corev1.Pod, spec *UpdateSpec) (string, error) {
	var newResourceVersion string
	retryErr := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		clone, err := c.podAdapter.GetPod(pod.Namespace, pod.Name)
		if err != nil {
			return err
		}

		// update new revision
		c.revisionAdapter.WriteRevisionHash(clone, spec.Revision)
		if clone.Annotations == nil {
			clone.Annotations = map[string]string{}
		}

		inPlaceUpdateState := InPlaceUpdateState{
			Revision:              spec.Revision,
			UpdateTimestamp:       metav1.NewTime(Clock.Now()),
			UpdateEnvFromMetadata: spec.UpdateEnvFromMetadata,
			UpdateImages:          len(spec.ContainerImages) > 0,
			UpdateResources:       len(spec.ContainerResources) > 0,
		}
		inPlaceUpdateStateJSON, _ := json.Marshal(inPlaceUpdateState)
		clone.Annotations[InPlaceUpdateStateKey] = string(inPlaceUpdateStateJSON)
		if clone, err = defaultPatchUpdateSpecToPod(clone, spec, &inPlaceUpdateState); err != nil {
			return err
		}

		newPod, updateErr := c.podAdapter.UpdatePod(clone)
		if updateErr == nil {
			newResourceVersion = newPod.ResourceVersion
		}
		return updateErr
	})
	return newResourceVersion, retryErr
}

func defaultPatchUpdateSpecToPod(pod *corev1.Pod, spec *UpdateSpec, state *InPlaceUpdateState) (*corev1.Pod, error) {
	klog.V(5).InfoS("Begin to in-place update pod", "namespace", pod.Namespace, "name", pod.Name, "spec", inplaceutils.DumpJSON(spec), "state", inplaceutils.DumpJSON(state))

	state.NextContainerImages = make(map[string]string)
	state.NextContainerRefMetadata = make(map[string]metav1.ObjectMeta)
	state.NextContainerResources = make(map[string]corev1.ResourceRequirements)

	if spec.MetaDataPatch != nil {
		cloneBytes, _ := json.Marshal(pod)
		modified, err := strategicpatch.StrategicMergePatch(cloneBytes, spec.MetaDataPatch, &corev1.Pod{})
		if err != nil {
			return nil, err
		}
		pod = &corev1.Pod{}
		if err = json.Unmarshal(modified, pod); err != nil {
			return nil, err
		}
	}

	if pod.Labels == nil {
		pod.Labels = make(map[string]string)
	}
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}

	// prepare containers that should update this time and next time, according to their priorities
	containersToUpdate := sets.NewString()
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		_, existImage := spec.ContainerImages[c.Name]
		_, existMetadata := spec.ContainerRefMetadata[c.Name]
		_, existResource := spec.ContainerResources[c.Name]
		if !existImage && !existMetadata && !existResource {
			continue
		}
		containersToUpdate.Insert(c.Name)
	}

	// update images and record current imageIDs for the containers to update
	containersImageChanged := sets.NewString()
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		newImage, exists := spec.ContainerImages[c.Name]
		if !exists {
			continue
		}
		if containersToUpdate.Has(c.Name) {
			pod.Spec.Containers[i].Image = newImage
			containersImageChanged.Insert(c.Name)
		} else {
			state.NextContainerImages[c.Name] = newImage
		}
	}
	for _, c := range pod.Status.ContainerStatuses {
		if containersImageChanged.Has(c.Name) {
			if state.LastContainerStatuses == nil {
				state.LastContainerStatuses = map[string]InPlaceUpdateContainerStatus{}
			}
			if iu, ok := state.LastContainerStatuses[c.Name]; !ok {
				state.LastContainerStatuses[c.Name] = InPlaceUpdateContainerStatus{ImageID: c.ImageID}
			} else {
				// now just update imageID
				iu.ImageID = c.ImageID
			}
		}
	}

	// update annotations and labels for the containers to update
	for cName, objMeta := range spec.ContainerRefMetadata {
		if containersToUpdate.Has(cName) {
			for k, v := range objMeta.Labels {
				pod.Labels[k] = v
			}
			for k, v := range objMeta.Annotations {
				pod.Annotations[k] = v
			}
		} else {
			state.NextContainerRefMetadata[cName] = objMeta
		}
	}

	// add the containers that update this time into PreCheckBeforeNext, so that next containers can only
	// start to update when these containers have updated ready
	// TODO: currently we only support ContainersRequiredReady, not sure if we have to add ContainersPreferredReady in future
	if len(state.NextContainerImages) > 0 || len(state.NextContainerRefMetadata) > 0 || len(state.NextContainerResources) > 0 {
		state.PreCheckBeforeNext = &InPlaceUpdatePreCheckBeforeNext{ContainersRequiredReady: containersToUpdate.List()}
	} else {
		state.PreCheckBeforeNext = nil
	}

	state.ContainerBatchesRecord = append(state.ContainerBatchesRecord, InPlaceUpdateContainerBatch{
		Timestamp:  metav1.NewTime(Clock.Now()),
		Containers: containersToUpdate.List(),
	})

	klog.V(5).InfoS("Decide to in-place update pod", "namespace", pod.Namespace, "name", pod.Name, "state", inplaceutils.DumpJSON(state))

	inPlaceUpdateStateJSON, _ := json.Marshal(state)
	pod.Annotations[InPlaceUpdateStateKey] = string(inPlaceUpdateStateJSON)
	return pod, nil
}
