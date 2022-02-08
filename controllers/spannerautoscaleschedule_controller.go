/*
Copyright 2022.

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

package controllers

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/go-logr/logr"
	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
)

// SpannerAutoscaleScheduleReconciler reconciles a SpannerAutoscaleSchedule object
type SpannerAutoscaleScheduleReconciler struct {
	ctrlClient ctrlclient.Client
	scheme     *runtime.Scheme
	recorder   record.EventRecorder
	log        logr.Logger
}

type SpannerAutoscaleScheduleReconcilerOption interface {
	applySpannerAutoscaleScheduleReconciler(r *SpannerAutoscaleScheduleReconciler)
}

func (o withLog) applySpannerAutoscaleScheduleReconciler(r *SpannerAutoscaleScheduleReconciler) {
	r.log = o.logger.WithName("schedule")
}

func NewSpannerAutoscaleScheduleReconciler(
	ctrlClient ctrlclient.Client,
	scheme *runtime.Scheme,
	recorder record.EventRecorder,
	opts ...Option,
) *SpannerAutoscaleScheduleReconciler {
	r := &SpannerAutoscaleScheduleReconciler{
		ctrlClient: ctrlClient,
		recorder:   recorder,
		scheme:     scheme,
	}

	for _, option := range opts {
		if opt, ok := option.(SpannerAutoscaleScheduleReconcilerOption); ok {
			opt.applySpannerAutoscaleScheduleReconciler(r)
		}
	}

	return r
}

//+kubebuilder:rbac:groups=spanner.mercari.com,resources=spannerautoscaleschedules,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=spanner.mercari.com,resources=spannerautoscaleschedules/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=spanner.mercari.com,resources=spannerautoscaleschedules/finalizers,verbs=update

//+kubebuilder:rbac:groups=spanner.mercari.com,resources=spannerautoscaler/status,verbs=get;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *SpannerAutoscaleScheduleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.log.WithValues("namespaced-name", req.NamespacedName, "emitter", "schedule")

	var sas spannerv1beta1.SpannerAutoscaleSchedule
	if err := r.ctrlClient.Get(ctx, req.NamespacedName, &sas); err != nil {
		// Ignore NotFound error, the resource might have been deleted
		err = ctrlclient.IgnoreNotFound(err)
		if err != nil {
			log.Error(err, "failed to get spanner-autoscale-schedule")
			return ctrlreconcile.Result{}, err
		}

		return ctrlreconcile.Result{}, nil
	}

	sasFinalizerName := "spannerautoscaleschedule.spanner.mercari.com/finalizer"

	// register our finalizer if the object is not being deleted
	if sas.ObjectMeta.DeletionTimestamp.IsZero() && !ctrlutil.ContainsFinalizer(&sas, sasFinalizerName) {
		log.V(1).Info("adding finalizer to schedule", "schedule", sas, "finalizer", sasFinalizerName)
		ctrlutil.AddFinalizer(&sas, sasFinalizerName)
		if err := r.ctrlClient.Update(ctx, &sas); err != nil {
			return ctrl.Result{}, err
		}
	}

	var sa spannerv1beta1.SpannerAutoscaler
	nnsa := types.NamespacedName{
		Name:      sas.Spec.TargetResource,
		Namespace: req.NamespacedName.Namespace,
	}
	err := r.ctrlClient.Get(ctx, nnsa, &sa)
	if err != nil && sas.ObjectMeta.DeletionTimestamp.IsZero() {
		// Do not ignore NotFound error, we need a valid SpannerAutoscaler
		log.Error(err, "failed to get spanner-autoscaler")
		r.recorder.Event(&sas, corev1.EventTypeWarning, "SpannerAutoscalerNotFound", err.Error())

		return ctrlreconcile.Result{}, err
	}

	// if the object is being deleted, perform clean up
	if !sas.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is being deleted
		if ctrlutil.ContainsFinalizer(&sas, sasFinalizerName) {
			// our finalizer is present, so if we found any autoscaler resource, then remove the schedule from the autoscaler list
			if len(sa.Status.Schedules) != 0 {
				sa.Status.Schedules = deleteFromArray(sa.Status.Schedules, req.NamespacedName.Name)
				log.Info("deleting schedule from spanner-autoscaler", "schedule", sas.Name, "autoscaler", sa.Name)
				if err := r.ctrlClient.Status().Update(ctx, &sa); err != nil {
					return ctrl.Result{}, err
				}
			}

			// remove our finalizer from the list and update it.
			ctrlutil.RemoveFinalizer(&sas, sasFinalizerName)
			if err := r.ctrlClient.Update(ctx, &sas); err != nil {
				return ctrl.Result{}, err
			}
		}

		// Stop reconciliation as the item is being deleted
		return ctrl.Result{}, nil
	}

	log.Info("registering schedule with spanner-autoscaler", "schedule", sas.Name, "autoscaler", sa.Name)

	if _, ok := findInArray(sa.Status.Schedules, req.NamespacedName.Name); !ok {
		sa.Status.Schedules = append(sa.Status.Schedules, req.NamespacedName.Name)
		if err := r.ctrlClient.Status().Update(ctx, &sa); err != nil {
			return ctrl.Result{}, err
		}
		log.V(1).Info("successfully registered schedule with spanner-autoscaler", "schedule", sas, "autoscaler", sa)
		r.recorder.Eventf(&sas, corev1.EventTypeNormal, "RegisteredWithSpannerAutoscaler", "successfully registered with spanner-autoscaler %q", sa.Name)
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SpannerAutoscaleScheduleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&spannerv1beta1.SpannerAutoscaleSchedule{}).
		Complete(r)
}

func findInArray(array []string, item string) (int, bool) {
	found := false
	index := -1
	for i, v := range array {
		if v == item {
			found = true
			index = i
			break
		}
	}
	return index, found
}

func deleteFromArray(array []string, item string) []string {
	result := []string{}
	for _, v := range array {
		if v != item {
			result = append(result, item)
		}
	}

	return result
}
