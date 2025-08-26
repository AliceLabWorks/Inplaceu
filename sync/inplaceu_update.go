package sync

import (
	"sort"
	"time"

	batchv1 "inplace.kubebuilder.io/project/api/v1"
	inplaceupdate "inplace.kubebuilder.io/project/inplace_update"
	inplacecore "inplace.kubebuilder.io/project/inplaceu_core"
	"inplace.kubebuilder.io/project/specifieddelete"
	inplaceutils "inplace.kubebuilder.io/project/utils"

	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

func (c *realControl) Update(iu *batchv1.Inplaceu,
	currentRevision, updateRevision *apps.ControllerRevision, revisions []*apps.ControllerRevision,
	pods []*corev1.Pod,
) error {

	key := inplaceutils.GetControllerKey(iu)
	coreControl := inplacecore.New(iu)

	// 1. calculate update diff and the revision to update
	diffRes := calculateDiffsWithExpectation(iu, pods, currentRevision.Name, updateRevision.Name)
	if diffRes.updateNum == 0 {
		return nil
	}

	// 2. find all matched pods can update
	var waitUpdateIndexes []int
	for i, pod := range pods {
		var waitUpdate, canUpdate bool
		waitUpdate = !inplaceutils.EqualToRevisionHash("", pod, updateRevision.Name)

		if waitUpdate {
			if pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodUnknown {
				canUpdate = true
			}
		}
		if canUpdate {
			waitUpdateIndexes = append(waitUpdateIndexes, i)
		}
	}

	// 3. sort all pods waiting to update
	sort.Slice(waitUpdateIndexes, coreControl.GetPodsSortFunc(pods, waitUpdateIndexes))

	// 4. limit max count of pods can update
	waitUpdateIndexes = limitUpdateIndexes(coreControl, diffRes, waitUpdateIndexes, pods, updateRevision.Name)

	// 5. update pods
	for _, idx := range waitUpdateIndexes {
		pod := pods[idx]
		duration, err := c.updatePod(iu, coreControl, updateRevision, revisions, pod)
		if duration > 0 {
			inplaceutils.DurationStore.Push(key, duration)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// limitUpdateIndexes limits all pods waiting update by the maxUnavailable policy, and returns the indexes of pods that can finally update
func limitUpdateIndexes(coreControl inplacecore.Control, diffRes expectationDiffs, waitUpdateIndexes []int, pods []*corev1.Pod, targetRevisionHash string) []int {
	if diffRes.updateNum < len(waitUpdateIndexes) {
		waitUpdateIndexes = waitUpdateIndexes[:diffRes.updateNum]
	}

	var unavailableCount, targetRevisionUnavailableCount, canUpdateCount int
	for _, p := range pods {
		if !inplaceutils.IsRunning(p) {
			unavailableCount++
			if inplaceutils.EqualToRevisionHash("", p, targetRevisionHash) {
				targetRevisionUnavailableCount++
			}
		}
	}
	for _, i := range waitUpdateIndexes {
		// Make sure unavailable pods in target revision should not be more than maxUnavailable.
		if targetRevisionUnavailableCount+canUpdateCount >= diffRes.updateMaxUnavailable {
			break
		}

		// Make sure unavailable pods in all revisions should not be more than maxUnavailable.
		// Note that update an old pod that already be unavailable will not increase the unavailable number.
		if inplaceutils.IsRunning(pods[i]) {
			if unavailableCount >= diffRes.updateMaxUnavailable {
				break
			}
			unavailableCount++
		}
		canUpdateCount++
	}

	if canUpdateCount < len(waitUpdateIndexes) {
		waitUpdateIndexes = waitUpdateIndexes[:canUpdateCount]
	}
	return waitUpdateIndexes
}

func (c *realControl) updatePod(iu *batchv1.Inplaceu, coreControl inplacecore.Control,
	updateRevision *apps.ControllerRevision, revisions []*apps.ControllerRevision,
	pod *corev1.Pod,
) (time.Duration, error) {
	var oldRevision *apps.ControllerRevision
	for _, r := range revisions {
		if inplaceutils.EqualToRevisionHash("", pod, r.Name) {
			oldRevision = r
			break
		}
	}
	if c.inplaceControl.CanUpdateInPlace(oldRevision, updateRevision) {
		res := c.inplaceControl.Update(pod, oldRevision, updateRevision)
		if res.InPlaceUpdate {
			if res.UpdateErr == nil {
				c.recorder.Eventf(iu, corev1.EventTypeNormal, "SuccessfulUpdatePodInPlace", "successfully update pod %s in-place(revision %v)", pod.Name, updateRevision.Name)
				inplaceutils.ResourceVersionExpectations.Expect(&metav1.ObjectMeta{UID: pod.UID, ResourceVersion: res.NewResourceVersion})
				return res.DelayDuration, nil
			}

			c.recorder.Eventf(iu, corev1.EventTypeWarning, "FailedUpdatePodInPlace", "failed to update pod %s in-place(revision %v): %v", pod.Name, updateRevision.Name, res.UpdateErr)
			return res.DelayDuration, res.UpdateErr
		}
	}

	klog.InfoS("inplaceU could not update Pod in-place, so it will back off to ReCreate", "inplaceU", klog.KObj(iu), "pod", klog.KObj(pod))

	klog.V(2).InfoS("inplaceU started to patch Pod specified-delete for update", "inplaceU", klog.KObj(iu), "pod", klog.KObj(pod), "updateRevision", klog.KObj(updateRevision))

	if patched, err := specifieddelete.PatchPodSpecifiedDelete(c.Client, pod, "true"); err != nil {
		c.recorder.Eventf(iu, v1.EventTypeWarning, "FailedUpdatePodReCreate",
			"failed to patch pod specified-delete %s for update(revision %s): %v", pod.Name, updateRevision.Name, err)
		return 0, err
	} else if patched {
		inplaceutils.ResourceVersionExpectations.Expect(pod)
	}

	c.recorder.Eventf(iu, corev1.EventTypeNormal, "SuccessfulUpdatePodReCreate",
		"successfully patch pod %s specified-delete for update(revision %s)", pod.Name, updateRevision.Name)
	return 0, nil
}

func (c *realControl) CanUpdateInPlace(oldRevision, newRevision *apps.ControllerRevision) bool {
	return inplaceupdate.CalculateSpec(oldRevision, newRevision) != nil
}
