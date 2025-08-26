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

package utils

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"

	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	kubecontroller "k8s.io/kubernetes/pkg/controller"
	"k8s.io/utils/integer"
	"sigs.k8s.io/controller-runtime/pkg/client"

	batchv1 "inplace.kubebuilder.io/project/api/v1"
	expectations "inplace.kubebuilder.io/project/utils/expectations"
	requeueduration "inplace.kubebuilder.io/project/utils/requeueduration"
)

var (
	// ControllerKind is GroupVersionKind for inplaceU.
	ControllerKind      = batchv1.GroupVersion.WithKind("Inplaceu")
	RevisionAdapterImpl = &revisionAdapterImpl{}
	EqualToRevisionHash = RevisionAdapterImpl.EqualToRevisionHash
	WriteRevisionHash   = RevisionAdapterImpl.WriteRevisionHash

	ScaleExpectations           = expectations.NewScaleExpectations()
	ResourceVersionExpectations = expectations.NewResourceVersionExpectation()

	// DurationStore is a short cut for any sub-functions to notify the reconcile how long to wait to requeue
	DurationStore = requeueduration.DurationStore{}
)

type revisionAdapterImpl struct {
}

func (r *revisionAdapterImpl) EqualToRevisionHash(_ string, obj metav1.Object, hash string) bool {
	objHash := obj.GetLabels()[apps.ControllerRevisionHashLabelKey]
	if objHash == hash {
		return true
	}
	return GetShortHash(hash) == GetShortHash(objHash)
}

func (r *revisionAdapterImpl) WriteRevisionHash(obj metav1.Object, hash string) {
	if obj.GetLabels() == nil {
		obj.SetLabels(make(map[string]string, 1))
	}
	// Note that controller-revision-hash defaults to be "{inplaceU_NAME}-{HASH}",
	// and it will be "{HASH}" if inplaceUShortHash feature-gate has been enabled.
	// But pod-template-hash should always be the short format.
	shortHash := GetShortHash(hash)
	obj.GetLabels()[apps.ControllerRevisionHashLabelKey] = hash
	obj.GetLabels()[apps.DefaultDeploymentUniqueLabelKey] = shortHash
}

func GetShortHash(hash string) string {
	// This makes sure the real hash must be the last '-' substring of revision name
	// vendor/k8s.io/kubernetes/pkg/controller/history/controller_history.go#82
	list := strings.Split(hash, "-")
	return list[len(list)-1]
}

// GetControllerKey return key of inplaceU.
func GetControllerKey(iu *batchv1.Inplaceu) string {
	return types.NamespacedName{Namespace: iu.Namespace, Name: iu.Name}.String()
}

// GetActiveAndInactivePods get activePods and inactivePods
func GetActiveAndInactivePods(reader client.Reader, opts *client.ListOptions) ([]*corev1.Pod, []*corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := reader.List(context.TODO(), podList, opts); err != nil {
		return nil, nil, err
	}
	var activePods, inactivePods []*corev1.Pod
	for i := range podList.Items {
		pod := &podList.Items[i]
		if kubecontroller.IsPodActive(pod) {
			activePods = append(activePods, pod)
		} else {
			inactivePods = append(inactivePods, pod)
		}
	}
	return activePods, inactivePods, nil
}

// NextRevision finds the next valid revision number based on revisions. If the length of revisions
// is 0 this is 1. Otherwise, it is 1 greater than the largest revision's Revision. This method
// assumes that revisions has been sorted by Revision.
func NextRevision(revisions []*apps.ControllerRevision) int64 {
	count := len(revisions)
	if count <= 0 {
		return 1
	}
	return revisions[count-1].Revision + 1
}

// IsRunningAndAvailable returns true if pod is in the PodRunning Phase, if it is available.
func IsRunning(pod *corev1.Pod) bool {
	return pod.Status.Phase == corev1.PodRunning
}

func IsPodAvailable(pod *corev1.Pod, minReadySeconds int32) bool {
	return pod.Status.Phase == corev1.PodRunning && podutil.IsPodAvailable(pod, minReadySeconds, metav1.Now())
}

func GroupUpdateAndNotUpdatePods(pods []*corev1.Pod, updateRevision string) (update, notUpdate []*corev1.Pod) {
	isPodUpdate := func(pod *corev1.Pod, updateRevision string) bool {
		return EqualToRevisionHash("", pod, updateRevision)
	}
	for _, p := range pods {
		if isPodUpdate(p, updateRevision) {
			update = append(update, p)
		} else {
			notUpdate = append(notUpdate, p)
		}
	}
	return
}

// DoItSlowly tries to call the provided function a total of 'count' times,
// starting slow to check for errors, then speeding up if calls succeed.
//
// It groups the calls into batches, starting with a group of initialBatchSize.
// Within each batch, it may call the function multiple times concurrently.
//
// If a whole batch succeeds, the next batch may get exponentially larger.
// If there are any failures in a batch, all remaining batches are skipped
// after waiting for the current batch to complete.
//
// It returns the number of successful calls to the function.
func DoItSlowly(count int, initialBatchSize int, fn func() error) (int, error) {
	remaining := count
	successes := 0
	for batchSize := integer.IntMin(remaining, initialBatchSize); batchSize > 0; batchSize = integer.IntMin(2*batchSize, remaining) {
		errCh := make(chan error, batchSize)
		var wg sync.WaitGroup
		wg.Add(batchSize)
		for i := 0; i < batchSize; i++ {
			go func() {
				defer wg.Done()
				if err := fn(); err != nil {
					errCh <- err
				}
			}()
		}
		wg.Wait()
		curSuccesses := batchSize - len(errCh)
		successes += curSuccesses
		if len(errCh) > 0 {
			return successes, <-errCh
		}
		remaining -= batchSize
	}
	return successes, nil
}

// DumpJSON returns the JSON encoding
func DumpJSON(o interface{}) string {
	j, _ := json.Marshal(o)
	return string(j)
}

// IsJSONObjectEqual checks if two objects are equal after encoding json
func IsJSONObjectEqual(o1, o2 interface{}) bool {
	if reflect.DeepEqual(o1, o2) {
		return true
	}

	oj1, _ := json.Marshal(o1)
	oj2, _ := json.Marshal(o2)
	os1 := string(oj1)
	os2 := string(oj2)
	if os1 == os2 {
		return true
	}

	om1 := make(map[string]interface{})
	om2 := make(map[string]interface{})
	_ = json.Unmarshal(oj1, &om1)
	_ = json.Unmarshal(oj2, &om2)

	return reflect.DeepEqual(om1, om2)
}

func GetPodsSortFunc(pods []*corev1.Pod, waitUpdateIndexes []int) func(i, j int) bool {
	// not-ready < ready, unscheduled < scheduled, and pending < running
	return func(i, j int) bool {
		return kubecontroller.ActivePods(pods).Less(waitUpdateIndexes[i], waitUpdateIndexes[j])
	}
}

func GetPodNames(pods []*corev1.Pod) sets.String {
	set := sets.NewString()
	for _, pod := range pods {
		set.Insert(pod.Name)
	}
	return set
}
