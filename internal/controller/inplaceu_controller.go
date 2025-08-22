/*
Copyright 2025.

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

package controller

import (
	"context"
	"time"

	apps "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/controller/history"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	batchv1 "inplace.kubebuilder.io/project/api/v1"
	eventhandler "inplace.kubebuilder.io/project/inplaceu_event_handler"
	inplacestatus "inplace.kubebuilder.io/project/status"
	synccontrol "inplace.kubebuilder.io/project/sync"
	"inplace.kubebuilder.io/project/utils"
	inplaceutils "inplace.kubebuilder.io/project/utils"
	inplacehistory "inplace.kubebuilder.io/project/utils/history"
	refmanager "inplace.kubebuilder.io/project/utils/refmgr"
	revisioncontrol "inplace.kubebuilder.io/project/utils/revision"
)

// InplaceuReconciler reconciles a Inplaceu object
type InplaceuReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	controllerHistory history.Interface
	revisionControl   revisioncontrol.Interface
	syncControl       synccontrol.Interface
	statusUpdater     inplacestatus.StatusUpdater
}

// +kubebuilder:rbac:groups=batch.inplace.kubebuilder.io,resources=inplaceus,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch.inplace.kubebuilder.io,resources=inplaceus/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch.inplace.kubebuilder.io,resources=inplaceus/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Inplaceu object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *InplaceuReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, retErr error) {
	logger := log.FromContext(ctx)
	logger.Info("reconcile Inplaceu")

	startTime := time.Now()
	defer func() {
		if retErr == nil {
			if res.RequeueAfter > 0 {
				klog.InfoS("Finished syncing CloneSet", "cloneSet", req, "cost", time.Since(startTime), "result", res)
			} else {
				klog.InfoS("Finished syncing CloneSet", "cloneSet", req, "cost", time.Since(startTime))
			}
		} else {
			klog.ErrorS(retErr, "Failed syncing CloneSet", "cloneSet", req)
		}
		// clean the duration store
		_ = utils.DurationStore.Pop(req.String())
	}()

	// 先拿到实例
	instance := &batchv1.Inplaceu{}
	err := r.Get(context.TODO(), req.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			klog.V(3).InfoS("CloneSet has been deleted", "cloneSet", req)
			// clonesetutils.ScaleExpectations.DeleteExpectations(request.String())
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	// 拿到selector
	selector, err := metav1.LabelSelectorAsSelector(instance.Spec.Selector)
	if err != nil {
		klog.ErrorS(err, "Error converting CloneSet selector", "cloneSet", req)
		// This is a non-transient error, so don't retry.
		return reconcile.Result{}, nil
	}

	// 如果上次扩缩容请求还没做完,那就等待
	if scaleSatisfied, unsatisfiedDuration, scaleDirtyPods := utils.ScaleExpectations.SatisfiedExpectations(req.String()); !scaleSatisfied {
		klog.V(4).InfoS("Not satisfied scale", "cloneSet", req, "scaleDirtyPods", scaleDirtyPods)
		return reconcile.Result{RequeueAfter: 1000 - unsatisfiedDuration}, nil
	}

	// 把所有属于当前iu的pods列出来
	filteredPods, _, err := r.getOwnedPods(instance)
	if err != nil {
		return reconcile.Result{}, err
	}

	// 释放不属于当前control控制的pod(可能有人手动把标签改了，需要把这些pod释放掉)
	filteredPods, err = r.claimPods(instance, filteredPods)
	if err != nil {
		return reconcile.Result{}, err
	}

	// 把属于自己的revision列出来
	// revision里记录了pod的template
	revisions, err := r.controllerHistory.ListControllerRevisions(instance, selector)
	if err != nil {
		return reconcile.Result{}, err
	}
	history.SortControllerRevisions(revisions)

	// get the current, and update revisions
	currentRevision, updateRevision, collisionCount, err := r.getActiveRevisions(instance, revisions)
	if err != nil {
		return reconcile.Result{}, err
	}
	// If resourceVersion expectations have not satisfied yet, just skip this reconcile
	utils.ResourceVersionExpectations.Observe(updateRevision)
	if isSatisfied, unsatisfiedDuration := utils.ResourceVersionExpectations.IsSatisfied(updateRevision); !isSatisfied {
		if unsatisfiedDuration < 5*time.Minute {
			klog.V(4).InfoS("Not satisfied resourceVersion for CloneSet, wait for updateRevision updating", "cloneSet", req, "updateRevisionName", updateRevision.Name)
			return reconcile.Result{RequeueAfter: 5*time.Minute - unsatisfiedDuration}, nil
		}
		klog.InfoS("Expectation unsatisfied overtime for CloneSet, wait for updateRevision updating timeout", "cloneSet", req, "updateRevisionName", updateRevision.Name, "timeout", unsatisfiedDuration)
		utils.ResourceVersionExpectations.Delete(updateRevision)
	}
	for _, pod := range filteredPods {
		utils.ResourceVersionExpectations.Observe(pod)
		if isSatisfied, unsatisfiedDuration := utils.ResourceVersionExpectations.IsSatisfied(pod); !isSatisfied {
			if unsatisfiedDuration >= 5*time.Minute {
				klog.InfoS("Expectation unsatisfied overtime for CloneSet, wait for pod updating timeout", "cloneSet", req, "pod", klog.KObj(pod), "timeout", unsatisfiedDuration)
				return reconcile.Result{}, nil
			}
			klog.V(4).InfoS("Not satisfied resourceVersion for CloneSet, wait for pod updating", "cloneSet", req, "pod", klog.KObj(pod))
			return reconcile.Result{RequeueAfter: 5*time.Minute - unsatisfiedDuration}, nil
		}
	}

	newStatus := batchv1.InplaceuStatus{
		ObservedGeneration: instance.Generation,
		CurrentRevision:    currentRevision.Name,
		UpdateRevision:     updateRevision.Name,
		CollisionCount:     new(int32),
	}
	*newStatus.CollisionCount = collisionCount

	// scale and update pods
	syncErr := r.syncInplaceu(instance, &newStatus, currentRevision, updateRevision, revisions, filteredPods)

	// update new status
	if err = r.statusUpdater.UpdateInplaceuSetStatus(instance, &newStatus, filteredPods); err != nil {
		return reconcile.Result{}, err
	}

	if err = r.truncateHistory(instance, filteredPods, revisions, currentRevision, updateRevision); err != nil {
		klog.ErrorS(err, "Failed to truncate history for CloneSet", "cloneSet", req)
	}

	if syncErr == nil && instance.Spec.MinReadySeconds > 0 && newStatus.AvailableReplicas != newStatus.ReadyReplicas {
		inplaceutils.DurationStore.Push(req.String(), time.Second*time.Duration(instance.Spec.MinReadySeconds))
	}
	return reconcile.Result{RequeueAfter: inplaceutils.DurationStore.Pop(req.String())}, syncErr
}

func (r *InplaceuReconciler) syncInplaceu(
	instance *batchv1.Inplaceu, newStatus *batchv1.InplaceuStatus,
	currentRevision, updateRevision *apps.ControllerRevision, revisions []*apps.ControllerRevision,
	filteredPods []*corev1.Pod,
) error {
	if instance.DeletionTimestamp != nil {
		return nil
	}

	// get the current and update revisions of the set.
	currentSet, err := r.revisionControl.ApplyRevision(instance, currentRevision)
	if err != nil {
		return err
	}
	updateSet, err := r.revisionControl.ApplyRevision(instance, updateRevision)
	if err != nil {
		return err
	}

	var scaling bool
	var podsScaleErr error
	var podsUpdateErr error

	scaling, podsScaleErr = r.syncControl.Scale(currentSet, updateSet, currentRevision.Name, updateRevision.Name, filteredPods)
	if podsScaleErr != nil {
		newStatus.Conditions = append(newStatus.Conditions, apps.DeploymentCondition{
			Type:               apps.DeploymentReplicaFailure,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Message:            podsScaleErr.Error(),
		})
		err = podsScaleErr
	}
	if scaling {
		return podsScaleErr
	}

	podsUpdateErr = r.syncControl.Update(updateSet, currentRevision, updateRevision, revisions, filteredPods)
	if podsUpdateErr != nil {
		newStatus.Conditions = append(newStatus.Conditions, apps.DeploymentCondition{
			Type:               apps.DeploymentReplicaFailure,
			Status:             corev1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Message:            podsUpdateErr.Error(),
		})
		if err == nil {
			err = podsUpdateErr
		}
	}

	return err
}

func (r *InplaceuReconciler) getOwnedPods(iu *batchv1.Inplaceu) ([]*corev1.Pod, []*corev1.Pod, error) {
	opts := &client.ListOptions{
		Namespace:     iu.Namespace,
		FieldSelector: fields.SelectorFromSet(fields.Set{"ownerRefUID": string(iu.UID)}),
	}
	return utils.GetActiveAndInactivePods(r.Client, opts)
}

func (r *InplaceuReconciler) claimPods(instance *batchv1.Inplaceu, pods []*corev1.Pod) ([]*corev1.Pod, error) {
	mgr, err := refmanager.New(r.Client, instance.Spec.Selector, instance, r.Scheme)
	if err != nil {
		return nil, err
	}

	selected := make([]metav1.Object, len(pods))
	for i, pod := range pods {
		selected[i] = pod
	}

	claimed, err := mgr.ClaimOwnedObjects(selected)
	if err != nil {
		return nil, err
	}

	claimedPods := make([]*corev1.Pod, len(claimed))
	for i, pod := range claimed {
		claimedPods[i] = pod.(*corev1.Pod)
	}

	return claimedPods, nil
}

func (r *InplaceuReconciler) getActiveRevisions(cs *batchv1.Inplaceu, revisions []*apps.ControllerRevision) (
	*apps.ControllerRevision, *apps.ControllerRevision, int32, error,
) {
	var currentRevision, updateRevision *apps.ControllerRevision
	revisionCount := len(revisions)

	// Use a local copy of cs.Status.CollisionCount to avoid modifying cs.Status directly.
	// This copy is returned so the value gets carried over to cs.Status in UpdateCloneSetStatus.
	var collisionCount int32
	if cs.Status.CollisionCount != nil {
		collisionCount = *cs.Status.CollisionCount
	}

	// create a new revision from the current cs
	updateRevision, err := r.revisionControl.NewRevision(cs, utils.NextRevision(revisions), &collisionCount)
	if err != nil {
		return nil, nil, collisionCount, err
	}

	// When there is a change in the PVC only, no new revision will be generated.
	// find any equivalent revisions
	equalRevisions := history.FindEqualRevisions(revisions, updateRevision)
	equalCount := len(equalRevisions)
	if equalCount > 0 && history.EqualRevision(revisions[revisionCount-1], equalRevisions[equalCount-1]) {
		// if the equivalent revision is immediately prior the update revision has not changed
		updateRevision = revisions[revisionCount-1]
	} else if equalCount > 0 {
		// if the equivalent revision is not immediately prior we will roll back by incrementing the
		// Revision of the equivalent revision
		updateRevision, err = r.controllerHistory.UpdateControllerRevision(equalRevisions[equalCount-1], updateRevision.Revision)
		if err != nil {
			return nil, nil, collisionCount, err
		}
	} else {
		//if there is no equivalent revision we create a new one
		updateRevision, err = r.controllerHistory.CreateControllerRevision(cs, updateRevision, &collisionCount)
		if err != nil {
			return nil, nil, collisionCount, err
		}
	}

	// attempt to find the revision that corresponds to the current revision
	for i := range revisions {
		if revisions[i].Name == cs.Status.CurrentRevision {
			currentRevision = revisions[i]
			break
		}
	}

	// if the current revision is nil we initialize the history by setting it to the update revision
	if currentRevision == nil {
		currentRevision = updateRevision
	}

	return currentRevision, updateRevision, collisionCount, nil
}

func (r *InplaceuReconciler) truncateHistory(
	cs *batchv1.Inplaceu,
	pods []*corev1.Pod,
	revisions []*apps.ControllerRevision,
	current *apps.ControllerRevision,
	update *apps.ControllerRevision,
) error {
	noLiveRevisions := make([]*apps.ControllerRevision, 0, len(revisions))

	// collect live revisions and historic revisions
	for i := range revisions {
		if revisions[i].Name != current.Name && revisions[i].Name != update.Name {
			var found bool
			for _, pod := range pods {
				if inplaceutils.EqualToRevisionHash("", pod, revisions[i].Name) {
					found = true
					break
				}
			}
			if !found {
				noLiveRevisions = append(noLiveRevisions, revisions[i])
			}
		}
	}
	historyLen := len(noLiveRevisions)
	historyLimit := 10
	if cs.Spec.RevisionHistoryLimit != nil {
		historyLimit = int(*cs.Spec.RevisionHistoryLimit)
	}
	if historyLen <= historyLimit {
		return nil
	}
	// delete any non-live history to maintain the revision limit.
	noLiveRevisions = noLiveRevisions[:(historyLen - historyLimit)]
	for i := 0; i < len(noLiveRevisions); i++ {
		if err := r.controllerHistory.DeleteControllerRevision(noLiveRevisions[i]); err != nil {
			return err
		}
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *InplaceuReconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.controllerHistory = inplacehistory.NewHistory(r.Client)
	r.revisionControl = revisioncontrol.NewRevisionControl()
	r.syncControl = synccontrol.New(r.Client, mgr.GetEventRecorderFor("inplaceu_controller"))
	r.statusUpdater = inplacestatus.NewStatusUpdater(r.Client)

	return ctrl.NewControllerManagedBy(mgr).For(&batchv1.Inplaceu{},
		builder.WithPredicates(
			predicate.Funcs{
				UpdateFunc: func(e event.UpdateEvent) bool {
					oldCS, okOld := e.ObjectOld.(*batchv1.Inplaceu)
					newCS, okNew := e.ObjectNew.(*batchv1.Inplaceu)
					if okOld && okNew {
						if oldCS.Spec.Replicas != nil && newCS.Spec.Replicas != nil &&
							*oldCS.Spec.Replicas != *newCS.Spec.Replicas {
							klog.V(4).InfoS("Observed updated replicas for CloneSet",
								"cloneSet", klog.KObj(newCS), "oldReplicas", *oldCS.Spec.Replicas, "newReplicas", *newCS.Spec.Replicas)
						}
					}
					return true // 这里return true表示无论如何都要入队
				},
			},
		),
	).
		Watches(
			&v1.Pod{},
			&eventhandler.PodEventHandler{Reader: r.Client},
		).Complete(r)
}
