package sync

import (
	"encoding/json"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	"k8s.io/utils/integer"

	batchv1 "inplace.kubebuilder.io/project/api/v1"
	"inplace.kubebuilder.io/project/utils"
)

type expectationDiffs struct {
	// scaleUpNum is a non-negative integer, which indicates the number that should scale up.
	scaleUpNum int
	// scaleDownNum is a non-negative integer, which indicates the number that should scale down.
	// It has excluded the number of Pods that are already specified to delete.
	scaleDownNum int

	// deleteReadyLimit is the limit number of ready Pods that can be deleted
	// it is limited by UpdateStrategy.maxUnavailable
	deleteReadyLimit int

	// useSurge is the number that temporarily expect to be above the desired replicas
	useSurge int

	// updateNum is the diff number that should update
	// '0' means no need to update
	// positive number means need to update more Pods to updateRevision
	updateNum int
	// updateMaxUnavailable is the maximum number of ready Pods that can be updating
	updateMaxUnavailable int
}

func (e expectationDiffs) isEmpty() bool {
	return reflect.DeepEqual(e, expectationDiffs{})
}

// String implement this to print information in klog
func (e expectationDiffs) String() string {
	b, _ := json.Marshal(e)
	return string(b)
}

func calculateDiffsWithExpectation(cs *batchv1.Inplaceu, pods []*corev1.Pod, currentRevision, updateRevision string) (res expectationDiffs) {
	replicas := int(*cs.Spec.Replicas)
	var maxSurge, maxUnavailable int
	if cs.Spec.Strategy.RollingUpdate.MaxSurge != nil {
		maxSurge, _ = intstrutil.GetValueFromIntOrPercent(cs.Spec.Strategy.RollingUpdate.MaxSurge, replicas, true)
	}
	maxUnavailable, _ = intstrutil.GetValueFromIntOrPercent(
		intstrutil.ValueOrDefault(cs.Spec.Strategy.RollingUpdate.MaxUnavailable, intstrutil.FromInt(0)), replicas, maxSurge == 0)

	var newRevisionCount, oldRevisionCount int
	var unavailableNewRevisionCount, unavailableOldRevisionCount int
	defer func() {
		if res.isEmpty() {
			return
		}
		klog.V(1).InfoS("Calculate diffs for CloneSet", "cloneSet", klog.KObj(cs), "replicas", replicas,
			"maxSurge", maxSurge, "maxUnavailable", maxUnavailable, "allPodCount", len(pods), "newRevisionCount", newRevisionCount,
			"oldrevisionCount", oldRevisionCount, "unavailableNewRevisionCount", unavailableNewRevisionCount,
			"unavailableOldRevisionCount", unavailableOldRevisionCount, "result", res.String())
	}()

	isPodUpdate := func(pod *corev1.Pod, updateRevision string) bool {
		return utils.EqualToRevisionHash("", pod, updateRevision)
	}

	for _, p := range pods {
		if isPodUpdate(p, updateRevision) {
			newRevisionCount++

			if !utils.IsRunning(p) {
				unavailableNewRevisionCount++
			}
		} else {
			oldRevisionCount++

			if !utils.IsRunning(p) {
				unavailableOldRevisionCount++
			}
		}
	}

	// 说明old需要删除,那就可以新增一些新版本,为了平滑升级
	if maxSurge > 0 && oldRevisionCount > 0 {
		res.useSurge = integer.IntMin(maxSurge, oldRevisionCount)
	}

	// prepare for scale calculation
	currentTotalCount := len(pods)
	expectedTotalCount := replicas + res.useSurge
	num := expectedTotalCount - currentTotalCount
	// scale up,扩容直接扩就完事了
	if num > 0 {
		res.scaleUpNum = num
	} else if num < 0 {
		// scale down
		res.scaleDownNum = num
		// 总的不可达数
		totalUnavailable := unavailableOldRevisionCount + unavailableNewRevisionCount
		res.deleteReadyLimit = integer.IntMax(maxUnavailable+(len(pods)-replicas)-totalUnavailable, 0)
	}

	// The consistency between scale and update will be guaranteed by syncCloneSet and expectations
	if oldRevisionCount != 0 {
		res.updateNum = oldRevisionCount
		res.updateMaxUnavailable = maxUnavailable + len(pods) - replicas
	}
	return
}
