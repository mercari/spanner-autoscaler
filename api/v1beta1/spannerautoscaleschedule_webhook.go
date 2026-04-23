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
	"context"
	"fmt"
	"time"

	cronpkg "github.com/robfig/cron/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// log is for logging in this package.
var spannerautoscaleschedulelog = logf.Log.WithName("spannerautoscaleschedule-resource.webhook")

func (r *SpannerAutoscaleSchedule) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, r).
		WithValidator(&spannerAutoscaleScheduleWebhook{}).
		Complete()
}

//+kubebuilder:webhook:path=/validate-spanner-mercari-com-v1beta1-spannerautoscaleschedule,mutating=false,failurePolicy=fail,sideEffects=None,groups=spanner.mercari.com,resources=spannerautoscaleschedules,verbs=create;update,versions=v1beta1,name=vspannerautoscaleschedule.kb.io,admissionReviewVersions=v1

// spannerAutoscaleScheduleWebhook implements admission.Validator for the
// SpannerAutoscaleSchedule resource.
type spannerAutoscaleScheduleWebhook struct{}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type.
func (*spannerAutoscaleScheduleWebhook) ValidateCreate(_ context.Context, obj *SpannerAutoscaleSchedule) (admission.Warnings, error) {
	spannerautoscaleschedulelog.Info("validate create", "name", obj.Name)
	spannerautoscaleschedulelog.V(1).Info("validating creation of SpannerAutoscaleSchedule resource", "name", obj.Name, "resource", obj)

	allErrs := validateSchedule(obj)

	if len(allErrs) == 0 {
		return nil, nil
	}

	return nil, apierrors.NewInvalid(
		schema.GroupKind{Group: "spanner.mercari.com", Kind: "SpannerAutoscaleSchedule"},
		obj.Name, allErrs)
}

// ValidateUpdate implements admission.Validator so a webhook will be registered for the type.
func (*spannerAutoscaleScheduleWebhook) ValidateUpdate(_ context.Context, oldObj, newObj *SpannerAutoscaleSchedule) (admission.Warnings, error) {
	spannerautoscaleschedulelog.Info("validate update", "name", newObj.Name)
	spannerautoscaleschedulelog.V(1).Info("validating updates to SpannerAutoscaleSchedule resource", "name", newObj.Name, "resource", newObj)

	if oldObj == nil {
		return nil, fmt.Errorf("expected old SpannerAutoscaleSchedule but got nil")
	}

	var allErrs field.ErrorList

	// TargetResource should not be changed after creation
	if oldObj.Spec.TargetResource != newObj.Spec.TargetResource {
		err := field.Invalid(
			field.NewPath("spec").Child("targetResource"),
			newObj.Spec.TargetResource,
			"'targetResource' can not be changed after resource has been created")
		allErrs = append(allErrs, err)
	}

	// Disable schedule updates until reconciler is modified to propagate this change to the actual cronjobs
	if oldObj.Spec.Schedule != newObj.Spec.Schedule {
		err := field.Invalid(
			field.NewPath("spec").Child("schedule"),
			newObj.Spec.Schedule,
			"'schedule' can not be changed after resource has been created")
		allErrs = append(allErrs, err)
	}

	allErrs = append(allErrs, validateSchedule(newObj)...)

	if len(allErrs) == 0 {
		return nil, nil
	}

	return nil, apierrors.NewInvalid(
		schema.GroupKind{Group: "spanner.mercari.com", Kind: "SpannerAutoscaleSchedule"},
		newObj.Name, allErrs)
}

// ValidateDelete implements admission.Validator so a webhook will be registered for the type.
func (*spannerAutoscaleScheduleWebhook) ValidateDelete(_ context.Context, obj *SpannerAutoscaleSchedule) (admission.Warnings, error) {
	spannerautoscaleschedulelog.Info("validate delete", "name", obj.Name)

	return nil, nil
}

func validateSchedule(r *SpannerAutoscaleSchedule) field.ErrorList {
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
