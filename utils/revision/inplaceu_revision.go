/*
Copyright 2019 The Kruise Authors.
Copyright 2016 The Kubernetes Authors.

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

package revision

import (
	"encoding/json"

	batchv1 "inplace.kubebuilder.io/project/api/v1"
	inplaceucore "inplace.kubebuilder.io/project/inplaceu_core"
	"inplace.kubebuilder.io/project/utils"

	apps "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/kubernetes/pkg/controller/history"
)

// Interface is a interface to new and apply ControllerRevision.
type Interface interface {
	NewRevision(iu *batchv1.Inplaceu, revision int64, collisionCount *int32) (*apps.ControllerRevision, error)
	ApplyRevision(iu *batchv1.Inplaceu, revision *apps.ControllerRevision) (*batchv1.Inplaceu, error)
}

// NewRevisionControl create a normal revision control.
func NewRevisionControl(scheme *runtime.Scheme) Interface {
	return &realControl{
		scheme: scheme,
	}
}

type realControl struct {
	scheme *runtime.Scheme
}

func (c *realControl) NewRevision(iu *batchv1.Inplaceu, revision int64, collisionCount *int32) (*apps.ControllerRevision, error) {
	coreControl := inplaceucore.New(iu)
	patch, err := c.getPatch(iu, coreControl)
	if err != nil {
		return nil, err
	}
	cr, err := history.NewControllerRevision(iu,
		utils.ControllerKind,
		iu.Spec.Template.Labels,
		runtime.RawExtension{Raw: patch},
		revision,
		collisionCount)
	if err != nil {
		return nil, err
	}
	if cr.ObjectMeta.Annotations == nil {
		cr.ObjectMeta.Annotations = make(map[string]string)
	}
	for key, value := range iu.Annotations {
		cr.ObjectMeta.Annotations[key] = value
	}
	return cr, nil
}

// getPatch returns a strategic merge patch that can be applied to restore a inplaceU to a
// previous version. If the returned error is nil the patch is valid. The current state that we save is just the
// PodSpecTemplate. We can modify this later to encompass more state (or less) and remain compatible with previously
// recorded patches.
func (c *realControl) getPatch(iu *batchv1.Inplaceu, coreControl inplaceucore.Control) ([]byte, error) {
	patchCodec := serializer.NewCodecFactory(c.scheme).LegacyCodec(batchv1.GroupVersion)
	str, err := runtime.Encode(patchCodec, iu)
	if err != nil {
		return nil, err
	}
	var raw map[string]interface{}
	_ = json.Unmarshal(str, &raw)
	objCopy := make(map[string]interface{})
	specCopy := make(map[string]interface{})
	spec := raw["spec"].(map[string]interface{})
	template := spec["template"].(map[string]interface{})

	coreControl.SetRevisionTemplate(specCopy, template)
	objCopy["spec"] = specCopy
	patch, err := json.Marshal(objCopy)
	return patch, err
}

func (c *realControl) ApplyRevision(iu *batchv1.Inplaceu, revision *apps.ControllerRevision) (*batchv1.Inplaceu, error) {
	clone := iu.DeepCopy()
	patchCodec := serializer.NewCodecFactory(c.scheme).LegacyCodec(batchv1.GroupVersion)
	cloneBytes, err := runtime.Encode(patchCodec, clone)
	if err != nil {
		return nil, err
	}
	patched, err := strategicpatch.StrategicMergePatch(cloneBytes, revision.Data.Raw, clone)
	if err != nil {
		return nil, err
	}
	coreControl := inplaceucore.New(clone)
	return coreControl.ApplyRevisionPatch(patched)
}
