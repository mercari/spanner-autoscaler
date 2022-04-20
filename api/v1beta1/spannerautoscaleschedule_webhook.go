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

package v1beta1

import (
	"time"

	cronpkg "github.com/robfig/cron/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// log is for logging in this package.
var spannerautoscaleschedulelog = logf.Log.WithName("spannerautoscaleschedule-resource.webhook")

func (r *SpannerAutoscaleSchedule) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		Complete()
}

//+kubebuilder:webhook:path=/validate-spanner-mercari-com-v1beta1-spannerautoscaleschedule,mutating=false,failurePolicy=fail,sideEffects=None,groups=spanner.mercari.com,resources=spannerautoscaleschedules,verbs=create;update,versions=v1beta1,name=vspannerautoscaleschedule.kb.io,admissionReviewVersions=v1

var _ webhook.Validator = &SpannerAutoscaleSchedule{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (r *SpannerAutoscaleSchedule) ValidateCreate() error {
	spannerautoscaleschedulelog.Info("validate create", "name", r.Name)
	spannerautoscaleschedulelog.V(1).Info("validating creation of SpannerAutoscaleSchedule resource", "name", r.Name, "resource", r)

	allErrs := r.validateSchedule()

	if len(allErrs) == 0 {
		return nil
	}

	return apierrors.NewInvalid(
		schema.GroupKind{Group: "spanner.mercari.com", Kind: "SpannerAutoscaleSchedule"},
		r.Name, allErrs)
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *SpannerAutoscaleSchedule) ValidateUpdate(old runtime.Object) error {
	spannerautoscaleschedulelog.Info("validate update", "name", r.Name)
	spannerautoscaleschedulelog.V(1).Info("validating updates to SpannerAutoscaleSchedule resource", "name", r.Name, "resource", r)

	var allErrs field.ErrorList
	oldResource := old.(*SpannerAutoscaleSchedule)

	// TargetResource should not be changed after creation
	if oldResource.Spec.TargetResource != r.Spec.TargetResource {
		err := field.Invalid(
			field.NewPath("spec").Child("targetResource"),
			r.Spec.TargetResource,
			"'targetResource' can not be changed after resource has been created")
		allErrs = append(allErrs, err)
	}

	// Disable schedule updates until reconciler is modified to propagate this change to the actual cronjobs
	if oldResource.Spec.Schedule != r.Spec.Schedule {
		err := field.Invalid(
			field.NewPath("spec").Child("schedule"),
			r.Spec.Schedule,
			"'schedule' can not be changed after resource has been created")
		allErrs = append(allErrs, err)
	}

	allErrs = append(allErrs, r.validateSchedule()...)

	if len(allErrs) == 0 {
		return nil
	}

	return apierrors.NewInvalid(
		schema.GroupKind{Group: "spanner.mercari.com", Kind: "SpannerAutoscaleSchedule"},
		r.Name, allErrs)
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *SpannerAutoscaleSchedule) ValidateDelete() error {
	spannerautoscaleschedulelog.Info("validate delete", "name", r.Name)

	// TODO(user): fill in your validation logic upon object deletion.
	return nil
}

func (r *SpannerAutoscaleSchedule) validateSchedule() field.ErrorList {
	var allErrs field.ErrorList

	if _, err := cronpkg.ParseStandard(r.Spec.Schedule.Cron); err != nil {
		fldErr := field.Invalid(
			field.NewPath("spec").Child("schedule").Child("cron"),
			r.Spec.Schedule.Cron,
			err.Error())
		allErrs = append(allErrs, fldErr)
	}

	if _, err := time.ParseDuration(r.Spec.Schedule.Duration); err != nil {
		fldErr := field.Invalid(
			field.NewPath("spec").Child("schedule").Child("duration"),
			r.Spec.Schedule.Duration,
			err.Error())
		allErrs = append(allErrs, fldErr)
	}

	return allErrs
}
