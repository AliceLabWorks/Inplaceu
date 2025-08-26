package sync

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	batchv1 "inplace.kubebuilder.io/project/api/v1"
	inplaceucore "inplace.kubebuilder.io/project/inplaceu_core"
	"inplace.kubebuilder.io/project/specifieddelete"
	inplaceutils "inplace.kubebuilder.io/project/utils"
	expectations "inplace.kubebuilder.io/project/utils/expectations"
)

const (
	// LengthOfInstanceID is the length of instance-id
	LengthOfInstanceID = 5

	// When batching pod creates, initialBatchSize is the size of the initial batch.
	initialBatchSize = 1
)

func (r *realControl) Scale(
	currentCS, updateCS *batchv1.Inplaceu,
	currentRevision, updateRevision string,
	pods []*corev1.Pod,
) (bool, error) {
	if updateCS.Spec.Replicas == nil {
		return false, fmt.Errorf("spec.Replicas is nil")
	}
	// 1. manage pods to delete
	podsSpecifiedToDelete, _ := getPlannedDeletedPods(pods)

	// 2. calculate scale numbers
	diffRes := calculateDiffsWithExpectation(updateCS, pods, currentRevision, updateRevision)
	updatedPods, _ := inplaceutils.GroupUpdateAndNotUpdatePods(pods, updateRevision)

	// 3. scale out
	if diffRes.scaleUpNum > 0 {
		// total number of this creation
		expectedCreations := diffRes.scaleUpNum

		klog.V(3).InfoS("inplaceU began to scale out pods, including current revision",
			"inplaceU", klog.KObj(updateCS), "expectedCreations", expectedCreations)

		// available instance-id come from free pvc
		availableIDs := getOrGenAvailableIDs(expectedCreations, pods)

		return r.createPods(expectedCreations,
			currentCS, updateCS, currentRevision, updateRevision, availableIDs.List())
	}

	// 4. specified delete
	if len(podsSpecifiedToDelete) > 0 {
		newPodsToDelete, oldPodsToDelete := inplaceutils.GroupUpdateAndNotUpdatePods(podsSpecifiedToDelete, updateRevision)
		klog.V(3).InfoS("inplaceU tried to delete pods specified", "inplaceU", klog.KObj(updateCS), "deleteReadyLimit", diffRes.deleteReadyLimit,
			"newPods", inplaceutils.GetPodNames(newPodsToDelete).List(), "oldPods", inplaceutils.GetPodNames(oldPodsToDelete).List())

		podsCanDelete := make([]*v1.Pod, 0, len(podsSpecifiedToDelete))
		for _, pod := range podsSpecifiedToDelete {
			if !inplaceutils.IsPodAvailable(pod, updateCS.Spec.MinReadySeconds) {
				podsCanDelete = append(podsCanDelete, pod)
			} else if diffRes.deleteReadyLimit > 0 {
				podsCanDelete = append(podsCanDelete, pod)
				diffRes.deleteReadyLimit--
			}
		}

		if modified, err := r.deletePods(updateCS, podsCanDelete); err != nil || modified {
			return modified, err
		}
	}

	// 5. scale in
	if diffRes.scaleDownNum > 0 {
		klog.V(3).InfoS("inplaceU began to scale in", "inplaceU", klog.KObj(updateCS), "scaleDownNum", diffRes.scaleDownNum,
			"deleteReadyLimit", diffRes.deleteReadyLimit)

		podsPreparingToDelete := r.choosePodsToDelete(updateCS, diffRes.scaleDownNum, updatedPods)
		podsToDelete := make([]*v1.Pod, 0, len(podsPreparingToDelete))
		for _, pod := range podsPreparingToDelete {
			if !inplaceutils.IsRunning(pod) {
				podsToDelete = append(podsToDelete, pod)
			} else if diffRes.deleteReadyLimit > 0 {
				podsToDelete = append(podsToDelete, pod)
				diffRes.deleteReadyLimit--
			}
		}

		return r.deletePods(updateCS, podsToDelete)
	}

	return false, nil
}

func (r *realControl) createPods(
	expectedCreations int,
	currentCS, updateCS *batchv1.Inplaceu,
	currentRevision, updateRevision string, availableIDs []string,
) (bool, error) {
	// new all pods need to create
	coreControl := inplaceucore.New(updateCS)
	newPods, err := coreControl.NewVersionedPods(updateCS, updateRevision, expectedCreations, availableIDs)
	if err != nil {
		return false, err
	}

	podsCreationChan := make(chan *v1.Pod, len(newPods))
	for _, p := range newPods {
		inplaceutils.ScaleExpectations.ExpectScale(inplaceutils.GetControllerKey(updateCS), expectations.Create, p.Name)
		podsCreationChan <- p
	}

	var created int64
	successPodNames := sync.Map{}
	_, err = inplaceutils.DoItSlowly(len(newPods), initialBatchSize, func() error {
		pod := <-podsCreationChan

		iu := updateCS
		var createErr error
		if createErr = r.createOnePod(iu, pod); createErr != nil {
			return createErr
		}

		atomic.AddInt64(&created, 1)

		successPodNames.Store(pod.Name, struct{}{})
		return nil
	})

	// rollback to ignore failure pods because the informer won't observe these pods
	for _, pod := range newPods {
		if _, ok := successPodNames.Load(pod.Name); !ok {
			inplaceutils.ScaleExpectations.ObserveScale(inplaceutils.GetControllerKey(updateCS), expectations.Create, pod.Name)
		}
	}

	if created == 0 {
		return false, err
	}
	return true, err
}

func getOrGenAvailableIDs(num int, pods []*v1.Pod) sets.String {
	existingIDs := sets.NewString()

	for _, pod := range pods {
		if id := pod.Labels[batchv1.InplaceuInstanceID]; len(id) > 0 {
			existingIDs.Insert(id)
		}
	}

	retIDs := sets.NewString()
	for i := 0; i < num; i++ {
		id := getOrGenInstanceID(existingIDs)
		retIDs.Insert(id)
	}

	return retIDs
}

func getOrGenInstanceID(existingIDs sets.String) string {
	var id string
	for {
		id = rand.String(LengthOfInstanceID)
		if !existingIDs.Has(id) {
			break
		}
	}
	return id
}

func (r *realControl) createOnePod(iu *batchv1.Inplaceu, pod *v1.Pod) error {
	if err := r.Create(context.TODO(), pod); err != nil {
		r.recorder.Eventf(iu, v1.EventTypeWarning, "FailedCreate", "failed to create pod: %v, pod: %v", err, inplaceutils.DumpJSON(pod))
		return err
	}

	r.recorder.Eventf(iu, v1.EventTypeNormal, "SuccessfulCreate", "succeed to create pod %s", pod.Name)
	return nil
}

func (r *realControl) choosePodsToDelete(iu *batchv1.Inplaceu, totalDiff int, updatedPods []*v1.Pod) []*v1.Pod {
	choose := func(pods []*v1.Pod, diff int) []*v1.Pod {
		// No need to sort pods if we are about to delete all of them.
		if diff < len(pods) {
			var ranker inplaceutils.Ranker
			ranker = inplaceutils.NewSameNodeRanker(pods)
			sort.Sort(inplaceutils.ActivePodsWithRanks{
				Pods:   pods,
				Ranker: ranker,
				AvailableFunc: func(pod *v1.Pod) bool {
					return inplaceutils.IsRunning(pod)
				},
			})
		} else if diff > len(pods) {
			klog.InfoS("Diff > len(pods) in choosePodsToDelete func which is not expected")
			return pods
		}
		return pods[:diff]
	}

	var podsToDelete []*v1.Pod
	podsToDelete = choose(updatedPods, totalDiff)

	return podsToDelete
}

func (r *realControl) deletePods(iu *batchv1.Inplaceu, podsToDelete []*v1.Pod) (bool, error) {
	var modified bool
	for _, pod := range podsToDelete {
		inplaceutils.ScaleExpectations.ExpectScale(inplaceutils.GetControllerKey(iu), expectations.Delete, pod.Name)
		if err := r.Delete(context.TODO(), pod); err != nil {
			inplaceutils.ScaleExpectations.ObserveScale(inplaceutils.GetControllerKey(iu), expectations.Delete, pod.Name)
			r.recorder.Eventf(iu, v1.EventTypeWarning, "FailedDelete", "failed to delete pod %s: %v", pod.Name, err)
			return modified, err
		}
		modified = true
		r.recorder.Event(iu, v1.EventTypeNormal, "SuccessfulDelete", fmt.Sprintf("succeed to delete pod %s", pod.Name))
	}

	return modified, nil
}

func getPlannedDeletedPods(pods []*v1.Pod) ([]*v1.Pod, int) {
	var podsSpecifiedToDelete []*v1.Pod
	names := sets.NewString()
	for _, pod := range pods {
		if specifieddelete.IsSpecifiedDelete(pod) {
			names.Insert(pod.Name)
			podsSpecifiedToDelete = append(podsSpecifiedToDelete, pod)
		}
	}
	return podsSpecifiedToDelete, names.Len()
}
