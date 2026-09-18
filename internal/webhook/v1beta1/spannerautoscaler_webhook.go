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
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	"github.com/mercari/spanner-autoscaler/internal/cron"
	"github.com/mercari/spanner-autoscaler/internal/scaling"
)

var log = logf.Log.WithName("spannerautoscaler-resource.webhook")

func SetupSpannerAutoscalerWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &spannerv1beta1.SpannerAutoscaler{}).
		WithDefaulter(&SpannerAutoscalerCustomDefaulter{}).
		WithValidator(&SpannerAutoscalerCustomValidator{}).
		Complete()
}

//+kubebuilder:webhook:path=/mutate-spanner-mercari-com-v1beta1-spannerautoscaler,mutating=true,failurePolicy=fail,sideEffects=None,groups=spanner.mercari.com,resources=spannerautoscalers,verbs=create;update,versions=v1beta1,name=mspannerautoscaler.kb.io,admissionReviewVersions=v1

type SpannerAutoscalerCustomDefaulter struct{}

// Default implements admission.CustomDefaulter so a webhook will be registered for the type.
func (*SpannerAutoscalerCustomDefaulter) Default(_ context.Context, obj *spannerv1beta1.SpannerAutoscaler) error {
	log.Info("default", "name", obj.Name)
	log.V(1).Info("received spannerautoscaler resource for setting defaults", "name", obj.Name, "resource", obj)

	// set the correct AuthType in Spec
	switch {
	case obj.Spec.Authentication.IAMKeySecret != nil:
		obj.Spec.Authentication.Type = spannerv1beta1.AuthTypeSA
	case obj.Spec.Authentication.ImpersonateConfig != nil:
		obj.Spec.Authentication.Type = spannerv1beta1.AuthTypeImpersonation
	default:
		obj.Spec.Authentication.Type = spannerv1beta1.AuthTypeADC
	}

	// set appropriate namespace for secret
	if obj.Spec.Authentication.Type == spannerv1beta1.AuthTypeSA && obj.Spec.Authentication.IAMKeySecret.Namespace == "" {
		obj.Spec.Authentication.IAMKeySecret.Namespace = obj.ObjectMeta.Namespace
	}

	// set default ComputeType
	if obj.Spec.ScaleConfig.ProcessingUnits != (spannerv1beta1.ScaleConfigPUs{}) {
		obj.Spec.ScaleConfig.ComputeType = spannerv1beta1.ComputeTypePU
	}

	// convert the nodes to processing units
	if obj.Spec.ScaleConfig.Nodes != (spannerv1beta1.ScaleConfigNodes{}) {
		obj.Spec.ScaleConfig.ComputeType = spannerv1beta1.ComputeTypeNode
		obj.Spec.ScaleConfig.ProcessingUnits = spannerv1beta1.ScaleConfigPUs{
			Min: obj.Spec.ScaleConfig.Nodes.Min * 1000,
			Max: obj.Spec.ScaleConfig.Nodes.Max * 1000,
		}
	}

	// set default ScaledownStepSize (only when truly unset).
	// IntValue() returns 0 for percent strings such as "10%", so the previous
	// `IntValue() == 0` check misclassified a percentage as "unset" and clobbered
	// it with 2000. Compare against the zero value instead so percent and integer
	// values are both preserved.
	if obj.Spec.ScaleConfig.ScaledownStepSize == (intstr.IntOrString{}) {
		obj.Spec.ScaleConfig.ScaledownStepSize = intstr.FromInt(2000)
	}

	log.V(1).Info("finished setting defaults for spannerautoscaler resource", "name", obj.Name, "resource", obj)
	return nil
}

//+kubebuilder:webhook:path=/validate-spanner-mercari-com-v1beta1-spannerautoscaler,mutating=false,failurePolicy=fail,sideEffects=None,groups=spanner.mercari.com,resources=spannerautoscalers,verbs=create;update,versions=v1beta1,name=vspannerautoscaler.kb.io,admissionReviewVersions=v1

type SpannerAutoscalerCustomValidator struct{}

// ValidateCreate implements admission.CustomValidator so a webhook will be registered for the type.
func (*SpannerAutoscalerCustomValidator) ValidateCreate(_ context.Context, obj *spannerv1beta1.SpannerAutoscaler) (admission.Warnings, error) {
	log.Info("validate create", "name", obj.Name)
	log.V(1).Info("validating creation of SpannerAutoscaler resource", "name", obj.Name, "resource", obj)

	allErrs := validateSpec(obj)

	if len(allErrs) == 0 {
		return nil, nil
	}

	return nil, apierrors.NewInvalid(
		schema.GroupKind{Group: "spanner.mercari.com", Kind: "SpannerAutoscaler"},
		obj.Name, allErrs)
}

// ValidateUpdate implements admission.CustomValidator so a webhook will be registered for the type.
func (*SpannerAutoscalerCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj *spannerv1beta1.SpannerAutoscaler) (admission.Warnings, error) {
	log.Info("validate update", "name", newObj.Name)
	log.V(1).Info("validating updates to SpannerAutoscaler resource", "name", newObj.Name, "resource", newObj)

	var allErrs field.ErrorList

	if oldObj == nil {
		return nil, fmt.Errorf("expected old SpannerAutoscaler but got nil")
	}

	if oldObj.Spec.TargetInstance != newObj.Spec.TargetInstance {
		err := field.Invalid(
			field.NewPath("spec").Child("targetInstance"),
			newObj.Spec.TargetInstance,
			"'targetInstance' is not allowed to be changed after resource has been created")
		allErrs = append(allErrs, err)
	}

	allErrs = append(allErrs, validateSpec(newObj)...)

	if len(allErrs) == 0 {
		return nil, nil
	}

	return nil, apierrors.NewInvalid(
		schema.GroupKind{Group: "spanner.mercari.com", Kind: "SpannerAutoscaler"},
		newObj.Name, allErrs)
}

// ValidateDelete implements admission.CustomValidator so a webhook will be registered for the type.
func (*SpannerAutoscalerCustomValidator) ValidateDelete(_ context.Context, obj *spannerv1beta1.SpannerAutoscaler) (admission.Warnings, error) {
	log.Info("validate delete", "name", obj.Name)

	return nil, nil
}

// ValidateSpec exposes the admission-time spec validation to non-webhook
// callers (the simulator validates generated candidates against the same
// rules a kubectl apply would hit) without the webhook's logging.
func ValidateSpec(r *spannerv1beta1.SpannerAutoscaler) error {
	allErrs := validateSpec(r)
	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: "spanner.mercari.com", Kind: "SpannerAutoscaler"},
		r.Name, allErrs)
}

func validateSpec(r *spannerv1beta1.SpannerAutoscaler) (allErrs field.ErrorList) {
	if err := validateAuthentication(r); err != nil {
		allErrs = append(allErrs, err)
	}
	if err := validateScaleConfig(r); err != nil {
		allErrs = append(allErrs, err)
	}
	allErrs = append(allErrs, validateCELScaleConfig(r)...)

	return allErrs
}

func validateAuthentication(r *spannerv1beta1.SpannerAutoscaler) *field.Error {
	if r.Spec.Authentication.ImpersonateConfig != nil && r.Spec.Authentication.IAMKeySecret != nil {
		return field.Invalid(
			field.NewPath("spec").Child("authentication"),
			r.Spec.Authentication,
			"'impersonateConfig' and 'iamKeySecret' are mutually exclusive values")
	}
	return nil
}

func validateTargetCPUUtilization(cpu spannerv1beta1.TargetCPUUtilization) *field.Error {
	if cpu.HighPriority == nil && cpu.Total == nil {
		return field.Required(
			field.NewPath("spec", "scaleConfig", "targetCPUUtilization"),
			"at least one of 'highPriority' or 'total' must be specified",
		)
	}
	return nil
}

func validateScaleConfig(r *spannerv1beta1.SpannerAutoscaler) *field.Error {
	sc := r.Spec.ScaleConfig

	if err := validateTargetCPUUtilization(sc.TargetCPUUtilization); err != nil {
		return err
	}

	if sc.Nodes != (spannerv1beta1.ScaleConfigNodes{}) {
		if sc.Nodes.Max < sc.Nodes.Min {
			return field.Invalid(
				field.NewPath("spec").Child("scaleConfig").Child("nodes").Child("max"),
				sc.Nodes.Max,
				"'max' value must be less than 'min' value")
		}
	}

	if sc.ProcessingUnits != (spannerv1beta1.ScaleConfigPUs{}) {
		if sc.ProcessingUnits.Max < sc.ProcessingUnits.Min {
			return field.Invalid(
				field.NewPath("spec").Child("scaleConfig").Child("processingUnits").Child("max"),
				sc.Nodes.Max,
				"'max' value must be less than 'min' value")
		}
		if err := validateProcessingUnits(
			sc.ProcessingUnits.Min,
			field.NewPath("spec").Child("scaleConfig").Child("processingUnits").Child("min")); err != nil {
			return err
		}
		if err := validateProcessingUnits(
			sc.ProcessingUnits.Max,
			field.NewPath("spec").Child("scaleConfig").Child("processingUnits").Child("max")); err != nil {
			return err
		}
	}

	switch sc.ScaledownStepSize.Type {
	case intstr.Int:
		if sc.ScaledownStepSize.IntVal > 1000 && sc.ScaledownStepSize.IntVal%1000 != 0 {
			return field.Invalid(
				field.NewPath("spec").Child("scaleConfig").Child("scaledownStepSize"),
				sc.ScaledownStepSize,
				"must be a multiple of 1000 for values which are greater than 1000")
		} else if sc.ScaledownStepSize.IntVal < 1000 && sc.ScaledownStepSize.IntVal%100 != 0 {
			return field.Invalid(
				field.NewPath("spec").Child("scaleConfig").Child("scaledownStepSize"),
				sc.ScaledownStepSize,
				"must be a multiple of 100 for values which are less than 1000")
		}
	case intstr.String:
		msg := validation.IsValidPercent(sc.ScaledownStepSize.StrVal)
		if len(msg) != 0 {
			return field.Invalid(
				field.NewPath("spec").Child("scaleConfig").Child("scaledownStepSize"),
				sc.ScaledownStepSize,
				strings.Join(msg, ", "))
		}
	default:
		return field.Invalid(
			field.NewPath("spec").Child("scaleConfig").Child("scaledownStepSize"),
			sc.ScaledownStepSize,
			"must be an integer or percentage (e.g '5%')")
	}

	switch sc.ScaleupStepSize.Type {
	case intstr.Int:
		if sc.ScaleupStepSize.IntVal > 1000 && sc.ScaleupStepSize.IntVal%1000 != 0 {
			return field.Invalid(
				field.NewPath("spec").Child("scaleConfig").Child("scaleupStepSize"),
				sc.ScaleupStepSize,
				"must be a multiple of 1000 for values which are greater than 1000")
		} else if sc.ScaleupStepSize.IntVal < 1000 && sc.ScaleupStepSize.IntVal%100 != 0 {
			return field.Invalid(
				field.NewPath("spec").Child("scaleConfig").Child("scaleupStepSize"),
				sc.ScaleupStepSize,
				"must be a multiple of 100 for values which are less than 1000")
		}
	case intstr.String:
		msg := validation.IsValidPercent(sc.ScaleupStepSize.StrVal)
		if len(msg) != 0 {
			return field.Invalid(
				field.NewPath("spec").Child("scaleConfig").Child("scaleupStepSize"),
				sc.ScaleupStepSize,
				strings.Join(msg, ", "))
		}
	default:
		return field.Invalid(
			field.NewPath("spec").Child("scaleConfig").Child("scaleupStepSize"),
			sc.ScaleupStepSize,
			"must be an integer or percentage (e.g '5%')")
	}

	// Validate mutual exclusion of scaledownAllowedTimes and scaledownNotAllowedTimes
	if len(sc.ScaledownAllowedTimes) > 0 && len(sc.ScaledownNotAllowedTimes) > 0 {
		return field.Forbidden(
			field.NewPath("spec").Child("scaleConfig"),
			"scaledownAllowedTimes and scaledownNotAllowedTimes cannot be specified together")
	}

	// Validate scaledownAllowedTimes cron expressions
	for i, cronExpr := range sc.ScaledownAllowedTimes {
		if _, err := cron.Parse(cronExpr); err != nil {
			return field.Invalid(
				field.NewPath("spec").Child("scaleConfig").Child("scaledownAllowedTimes").Index(i),
				cronExpr,
				fmt.Sprintf("invalid cron expression: %v", err))
		}
	}

	// Validate scaledownNotAllowedTimes cron expressions
	for i, cronExpr := range sc.ScaledownNotAllowedTimes {
		if _, err := cron.Parse(cronExpr); err != nil {
			return field.Invalid(
				field.NewPath("spec").Child("scaleConfig").Child("scaledownNotAllowedTimes").Index(i),
				cronExpr,
				fmt.Sprintf("invalid cron expression: %v", err))
		}
	}

	return nil
}

// validateCELScaleConfig validates the CEL-based scaling fields:
// metricWindows, scalingRules, scaleupCondition and scaledownCondition.
// Expressions are compiled (and type-checked to return a boolean) against the
// exact variable set the controller will evaluate them with, so a typo, a
// reference to an undeclared window, or a reference to a CPU metric that has
// no target configured is rejected at apply time.
func validateCELScaleConfig(r *spannerv1beta1.SpannerAutoscaler) (allErrs field.ErrorList) {
	sc := r.Spec.ScaleConfig
	scPath := field.NewPath("spec").Child("scaleConfig")

	windowsPath := scPath.Child("metricWindows")
	if len(sc.MetricWindows) > scaling.MaxMetricWindows {
		allErrs = append(allErrs, field.TooMany(windowsPath, len(sc.MetricWindows), scaling.MaxMetricWindows))
	}
	// Deduplicate by parsed duration, not by spelling: "60m" and "1h" declare
	// the same window, and the syncer maps computed aggregates back to one
	// spelling — the other's CEL variables would never be populated.
	seen := make(map[time.Duration]string, len(sc.MetricWindows))
	for i, w := range sc.MetricWindows {
		d, err := scaling.ParseMetricWindow(w)
		if err != nil {
			allErrs = append(allErrs, field.Invalid(windowsPath.Index(i), w, err.Error()))
			continue
		}
		if prev, ok := seen[d]; ok {
			msg := w
			if prev != w {
				msg = fmt.Sprintf("%s (same duration as %q)", w, prev)
			}
			allErrs = append(allErrs, field.Duplicate(windowsPath.Index(i), msg))
			continue
		}
		seen[d] = w
	}

	flags := sc.TargetCPUUtilization.ActiveMetricFlags()
	windows := scaling.ValidMetricWindows(sc.MetricWindows)

	for i, rule := range sc.ScalingRules {
		rulePath := scPath.Child("scalingRules").Index(i)
		if rule.When == "" {
			allErrs = append(allErrs, field.Required(rulePath.Child("when"), "a CEL expression is required"))
		} else if err := scaling.CompileCondition(flags, windows, rule.When); err != nil {
			allErrs = append(allErrs, field.Invalid(rulePath.Child("when"), rule.When, err.Error()))
		}
		if err := validateScaleUpAmount(rule.ScaleUp, rulePath.Child("scaleUp")); err != nil {
			allErrs = append(allErrs, err)
		}
	}

	if sc.ScaleupCondition != "" {
		if err := scaling.CompileCondition(flags, windows, sc.ScaleupCondition); err != nil {
			allErrs = append(allErrs, field.Invalid(scPath.Child("scaleupCondition"), sc.ScaleupCondition, err.Error()))
		}
	}
	if sc.ScaledownCondition != "" {
		if err := scaling.CompileCondition(flags, windows, sc.ScaledownCondition); err != nil {
			allErrs = append(allErrs, field.Invalid(scPath.Child("scaledownCondition"), sc.ScaledownCondition, err.Error()))
		}
	}

	return allErrs
}

// validateScaleUpAmount validates a scalingRules[].scaleUp value: a positive
// number of processing units on the usual 100/1000 grid, or a percentage of
// the current processing units between 1% and 100% (at most doubling per
// trigger, in line with the Spanner guideline of scaling by at most 2x per
// change).
func validateScaleUpAmount(scaleUp intstr.IntOrString, fldPath *field.Path) *field.Error {
	switch scaleUp.Type {
	case intstr.Int:
		v := scaleUp.IntValue()
		switch {
		case v <= 0:
			return field.Invalid(fldPath, scaleUp, "must be positive")
		case v > 1000 && v%1000 != 0:
			return field.Invalid(fldPath, scaleUp, "must be a multiple of 1000 for values which are greater than 1000")
		case v < 1000 && v%100 != 0:
			return field.Invalid(fldPath, scaleUp, "must be a multiple of 100 for values which are less than 1000")
		}
	case intstr.String:
		if msg := validation.IsValidPercent(scaleUp.StrVal); len(msg) != 0 {
			return field.Invalid(fldPath, scaleUp, strings.Join(msg, ", "))
		}
		percent, err := strconv.Atoi(strings.TrimSuffix(scaleUp.StrVal, "%"))
		if err != nil || percent < 1 || percent > 100 {
			return field.Invalid(fldPath, scaleUp, "percentage must be between 1% and 100%")
		}
	default:
		return field.Invalid(fldPath, scaleUp, "must be an integer or percentage (e.g '25%')")
	}
	return nil
}

func validateProcessingUnits(pu int, fldPath *field.Path) *field.Error {
	if pu >= 1000 && pu%1000 != 0 {
		return field.Invalid(
			fldPath,
			pu,
			"processing units which are greater than 1000, should be multiples of 1000")
	}

	return nil
}
