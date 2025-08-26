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
	"encoding/json"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	"k8s.io/utils/integer"

	batchv1 "inplace.kubebuilder.io/project/api/v1"
	"inplace.kubebuilder.io/project/internal/specifieddelete"
	"inplace.kubebuilder.io/project/internal/utils"
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

func calculateDiffsWithExpectation(iu *batchv1.Inplaceu, pods []*corev1.Pod, currentRevision, updateRevision string) (res expectationDiffs) {
	replicas := int(*iu.Spec.Replicas)
	var maxSurge, maxUnavailable int
	if iu.Spec.Strategy.RollingUpdate.MaxSurge != nil {
		maxSurge, _ = intstrutil.GetValueFromIntOrPercent(iu.Spec.Strategy.RollingUpdate.MaxSurge, replicas, true)
	}
	maxUnavailable, _ = intstrutil.GetValueFromIntOrPercent(
		intstrutil.ValueOrDefault(iu.Spec.Strategy.RollingUpdate.MaxUnavailable, intstrutil.FromInt(0)), replicas, maxSurge == 0)

	var newRevisionCount, oldRevisionCount int
	var unavailableNewRevisionCount, unavailableOldRevisionCount int
	var toDeleteNewRevisionCount, toDeleteOldRevisionCount int
	defer func() {
		if res.isEmpty() {
			return
		}
		klog.V(1).InfoS("Calculate diffs for inplaceU", "inplaceU", klog.KObj(iu), "replicas", replicas,
			"maxSurge", maxSurge, "maxUnavailable", maxUnavailable, "allPodCount", len(pods), "newRevisionCount", newRevisionCount,
			"oldrevisionCount", oldRevisionCount, "unavailableNewRevisionCount", unavailableNewRevisionCount,
			"unavailableOldRevisionCount", unavailableOldRevisionCount, "toDeleteOldRevisionCount", toDeleteOldRevisionCount,
			"toDeleteNewRevisionCount", toDeleteNewRevisionCount, "result", res.String())
	}()

	isPodUpdate := func(pod *corev1.Pod, updateRevision string) bool {
		return utils.EqualToRevisionHash("", pod, updateRevision)
	}

	for _, p := range pods {
		if isPodUpdate(p, updateRevision) {
			newRevisionCount++

			if specifieddelete.IsSpecifiedDelete(p) {
				toDeleteNewRevisionCount++
			} else if !utils.IsRunning(p) {
				unavailableNewRevisionCount++
			}
		} else {
			oldRevisionCount++

			if specifieddelete.IsSpecifiedDelete(p) {
				toDeleteOldRevisionCount++
			} else if !utils.IsRunning(p) {
				unavailableOldRevisionCount++
			}
		}
	}

	if maxSurge > 0 {
		// 有需要特别删除的，就补一些,平滑升级
		if toDeleteCount := toDeleteNewRevisionCount + toDeleteOldRevisionCount; toDeleteCount > 0 {
			res.useSurge = toDeleteCount
		}
		// 说明有一些老版本需要升级为新版本,补一些,平滑升级
		if oldRevisionCount > toDeleteOldRevisionCount {
			res.useSurge = integer.IntMax(res.useSurge, oldRevisionCount-toDeleteOldRevisionCount)
		}
		// 限制一下
		res.useSurge = integer.IntMin(res.useSurge, maxSurge)
	}

	// prepare for scale calculation
	currentTotalCount := len(pods)
	expectedTotalCount := replicas + res.useSurge
	num := expectedTotalCount - currentTotalCount
	// scale up,扩容直接扩就完事了
	if num > 0 {
		res.scaleUpNum = num
	} else if downNum := currentTotalCount - expectedTotalCount - toDeleteNewRevisionCount - toDeleteOldRevisionCount; downNum > 0 {
		// scale down
		res.scaleDownNum = downNum
	}

	if toDeleteNewRevisionCount > 0 || toDeleteOldRevisionCount > 0 || res.scaleDownNum > 0 {
		// 总的不可达数
		totalUnavailable := unavailableOldRevisionCount + unavailableNewRevisionCount
		res.deleteReadyLimit = integer.IntMax(maxUnavailable+(len(pods)-replicas)-totalUnavailable, 0)
	}

	// The consistency between scale and update will be guaranteed by syncinplaceU and expectations
	// 其实在update时如果走到这里,toDeleteOldRevisionCount应该是0
	if oldRevisionCount > toDeleteOldRevisionCount {
		res.updateNum = oldRevisionCount - toDeleteOldRevisionCount
		res.updateMaxUnavailable = maxUnavailable + len(pods) - replicas
	}
	return
}
