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

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/go-logr/logr"
	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
)

// SpannerAutoscaleScheduleReconciler reconciles a SpannerAutoscaleSchedule object
type SpannerAutoscaleScheduleReconciler struct {
	ctrlClient ctrlclient.Client
	scheme     *runtime.Scheme
	log        logr.Logger
}

//+kubebuilder:rbac:groups=spanner.mercari.com,resources=spannerautoscaleschedules,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=spanner.mercari.com,resources=spannerautoscaleschedules/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=spanner.mercari.com,resources=spannerautoscaleschedules/finalizers,verbs=update

//+kubebuilder:rbac:groups=spanner.mercari.com,resources=spannerautoscaler/status,verbs=get;update;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *SpannerAutoscaleScheduleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	nn := req.NamespacedName
	log := r.log.WithValues("namespaced-name", nn)

	var sas spannerv1beta1.SpannerAutoscaleSchedule
	if err := r.ctrlClient.Get(ctx, nn, &sas); err != nil {
		err = ctrlclient.IgnoreNotFound(err)
		if err != nil {
			log.Error(err, "failed to get spanner-autoscale-schedule")
			return ctrlreconcile.Result{}, err
		}

		return ctrlreconcile.Result{}, nil
	}

	var sa spannerv1beta1.SpannerAutoscaler
	nnsa := types.NamespacedName{
		Name:      sas.Spec.TargetResource,
		Namespace: nn.Namespace,
	}
	if err := r.ctrlClient.Get(ctx, nnsa, &sa); err != nil {
		err = ctrlclient.IgnoreNotFound(err)
		if err != nil {
			log.Error(err, "failed to get spanner-autoscaler")
			return ctrlreconcile.Result{}, err
		}

		return ctrlreconcile.Result{}, nil
	}

	if _, ok := findInArray(sa.Status.Schedules, nn.Name); !ok {
		sa.Status.Schedules = append(sa.Status.Schedules, nn.Name)

		if err := r.ctrlClient.Status().Update(ctx, &sa); err != nil {
			return ctrl.Result{}, err
		}
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
