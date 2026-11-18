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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	"github.com/mercari/spanner-autoscaler/internal/cron"
)

var log = logf.Log.WithName("spannerautoscaler-resource.webhook")

// DefaultMaxTimeBasedOverrides is the default limit on the number of
// spec.scaleConfig.timeBasedOverrides entries.
const DefaultMaxTimeBasedOverrides = 10

// SpannerAutoscalerWebhookOption configures the SpannerAutoscaler webhook.
type SpannerAutoscalerWebhookOption func(*SpannerAutoscalerCustomValidator)

// WithMaxTimeBasedOverrides sets the limit on the number of
// spec.scaleConfig.timeBasedOverrides entries. 0 or less disables the limit.
func WithMaxTimeBasedOverrides(n int) SpannerAutoscalerWebhookOption {
	return func(v *SpannerAutoscalerCustomValidator) {
		v.MaxTimeBasedOverrides = n
	}
}

func SetupSpannerAutoscalerWebhookWithManager(mgr ctrl.Manager, opts ...SpannerAutoscalerWebhookOption) error {
	validator := &SpannerAutoscalerCustomValidator{MaxTimeBasedOverrides: DefaultMaxTimeBasedOverrides}
	for _, opt := range opts {
		opt(validator)
	}
	return ctrl.NewWebhookManagedBy(mgr, &spannerv1beta1.SpannerAutoscaler{}).
		WithDefaulter(&SpannerAutoscalerCustomDefaulter{}).
		WithValidator(validator).
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

type SpannerAutoscalerCustomValidator struct {
	// MaxTimeBasedOverrides limits the number of spec.scaleConfig.timeBasedOverrides entries.
	// 0 or less disables the limit. It is not a CRD MaxItems so that operators can raise it
	// with a controller flag.
	MaxTimeBasedOverrides int
}

// ValidateCreate implements admission.CustomValidator so a webhook will be registered for the type.
func (v *SpannerAutoscalerCustomValidator) ValidateCreate(_ context.Context, obj *spannerv1beta1.SpannerAutoscaler) (admission.Warnings, error) {
	log.Info("validate create", "name", obj.Name)
	log.V(1).Info("validating creation of SpannerAutoscaler resource", "name", obj.Name, "resource", obj)

	allErrs := validateSpec(obj, v.MaxTimeBasedOverrides)

	if len(allErrs) == 0 {
		return nil, nil
	}

	return nil, apierrors.NewInvalid(
		schema.GroupKind{Group: "spanner.mercari.com", Kind: "SpannerAutoscaler"},
		obj.Name, allErrs)
}

// ValidateUpdate implements admission.CustomValidator so a webhook will be registered for the type.
func (v *SpannerAutoscalerCustomValidator) ValidateUpdate(_ context.Context, oldObj, newObj *spannerv1beta1.SpannerAutoscaler) (admission.Warnings, error) {
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

	allErrs = append(allErrs, validateSpec(newObj, v.MaxTimeBasedOverrides)...)

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
// The limit on the number of time-based overrides is not checked, because
// it depends on a controller flag and a replay should accept any manifest
// the cluster accepted.
func ValidateSpec(r *spannerv1beta1.SpannerAutoscaler) error {
	allErrs := validateSpec(r, 0)
	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: "spanner.mercari.com", Kind: "SpannerAutoscaler"},
		r.Name, allErrs)
}

func validateSpec(r *spannerv1beta1.SpannerAutoscaler, maxTimeBasedOverrides int) (allErrs field.ErrorList) {
	if err := validateAuthentication(r); err != nil {
		allErrs = append(allErrs, err)
	}
	if err := validateScaleConfig(r); err != nil {
		allErrs = append(allErrs, err)
	}
	allErrs = append(allErrs, validateTimeBasedOverrides(r.Spec.ScaleConfig, maxTimeBasedOverrides)...)

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

	if err := validateStepSize(sc.ScaledownStepSize, field.NewPath("spec").Child("scaleConfig").Child("scaledownStepSize")); err != nil {
		return err
	}

	if err := validateStepSize(sc.ScaleupStepSize, field.NewPath("spec").Child("scaleConfig").Child("scaleupStepSize")); err != nil {
		return err
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

func validateStepSize(v intstr.IntOrString, fldPath *field.Path) *field.Error {
	switch v.Type {
	case intstr.Int:
		if v.IntVal > 1000 && v.IntVal%1000 != 0 {
			return field.Invalid(fldPath, v, "must be a multiple of 1000 for values which are greater than 1000")
		} else if v.IntVal < 1000 && v.IntVal%100 != 0 {
			return field.Invalid(fldPath, v, "must be a multiple of 100 for values which are less than 1000")
		}
	case intstr.String:
		if msg := validation.IsValidPercent(v.StrVal); len(msg) != 0 {
			return field.Invalid(fldPath, v, strings.Join(msg, ", "))
		}
	default:
		return field.Invalid(fldPath, v, "must be an integer or percentage (e.g '5%')")
	}
	return nil
}

func validateTimeBasedOverrides(sc spannerv1beta1.ScaleConfig, maxCount int) (allErrs field.ErrorList) {
	if len(sc.TimeBasedOverrides) == 0 {
		return nil
	}
	path := field.NewPath("spec").Child("scaleConfig").Child("timeBasedOverrides")

	// One mechanism decides whether scale down is allowed at a given minute.
	// Relaxing this later is backward compatible; tightening it would not be.
	if len(sc.ScaledownAllowedTimes) > 0 || len(sc.ScaledownNotAllowedTimes) > 0 {
		allErrs = append(allErrs, field.Forbidden(path,
			"cannot be used together with scaledownAllowedTimes or scaledownNotAllowedTimes; "+
				"express the time restriction with scaledownPolicy in timeBasedOverrides"))
	}

	if maxCount > 0 && len(sc.TimeBasedOverrides) > maxCount {
		allErrs = append(allErrs, field.TooMany(path, len(sc.TimeBasedOverrides), maxCount))
	}

	names := make(map[string]bool, len(sc.TimeBasedOverrides))
	for i, o := range sc.TimeBasedOverrides {
		p := path.Index(i)
		if o.Name == "" {
			allErrs = append(allErrs, field.Required(p.Child("name"), ""))
		} else if names[o.Name] {
			allErrs = append(allErrs, field.Duplicate(p.Child("name"), o.Name))
		}
		names[o.Name] = true
		allErrs = append(allErrs, validateTimeBasedOverride(sc, o, p)...)
	}

	return allErrs
}

func validateTimeBasedOverride(sc spannerv1beta1.ScaleConfig, o spannerv1beta1.TimeBasedOverride, p *field.Path) (allErrs field.ErrorList) {
	if len(o.Times) == 0 {
		allErrs = append(allErrs, field.Required(p.Child("times"), ""))
	}
	for j, expr := range o.Times {
		if _, err := cron.Parse(expr); err != nil {
			allErrs = append(allErrs, field.Invalid(p.Child("times").Index(j), expr, fmt.Sprintf("invalid cron expression: %v", err)))
		}
	}

	if o.ProcessingUnits == nil && o.TargetCPUUtilization == nil &&
		o.ScaledownStepSize == nil && o.ScaledownInterval == nil &&
		o.ScaleupStepSize == nil && o.ScaleupInterval == nil &&
		o.ScaledownPolicy == "" {
		allErrs = append(allErrs, field.Required(p, "at least one field other than name and times must be specified"))
	}

	if o.ProcessingUnits != nil {
		allErrs = append(allErrs, validateOverridePUs(sc.ProcessingUnits, *o.ProcessingUnits, p.Child("processingUnits"))...)
	}
	if o.TargetCPUUtilization != nil {
		allErrs = append(allErrs, validateOverrideTarget(sc.TargetCPUUtilization, *o.TargetCPUUtilization, p.Child("targetCPUUtilization"))...)
	}

	for _, step := range []struct {
		name  string
		value *intstr.IntOrString
	}{{"scaledownStepSize", o.ScaledownStepSize}, {"scaleupStepSize", o.ScaleupStepSize}} {
		if step.value == nil {
			continue
		}
		if err := validateStepSize(*step.value, p.Child(step.name)); err != nil {
			allErrs = append(allErrs, err)
		}
	}
	for _, interval := range []struct {
		name  string
		value *metav1.Duration
	}{{"scaledownInterval", o.ScaledownInterval}, {"scaleupInterval", o.ScaleupInterval}} {
		if interval.value != nil && interval.value.Duration <= 0 {
			allErrs = append(allErrs, field.Invalid(p.Child(interval.name), interval.value.Duration.String(), "must be positive"))
		}
	}

	switch o.ScaledownPolicy {
	case "", spannerv1beta1.ScaledownPolicyAllow, spannerv1beta1.ScaledownPolicyDeny:
	default:
		allErrs = append(allErrs, field.NotSupported(p.Child("scaledownPolicy"), o.ScaledownPolicy,
			[]spannerv1beta1.ScaledownPolicy{spannerv1beta1.ScaledownPolicyAllow, spannerv1beta1.ScaledownPolicyDeny}))
	}

	return allErrs
}

func validateOverridePUs(base spannerv1beta1.ScaleConfigPUs, pu spannerv1beta1.TimeBasedOverridePUs, p *field.Path) (allErrs field.ErrorList) {
	effMin, effMax := base.Min, base.Max
	for _, bound := range []struct {
		name      string
		value     *int
		effective *int
	}{{"min", pu.Min, &effMin}, {"max", pu.Max, &effMax}} {
		if bound.value == nil {
			continue
		}
		if *bound.value < 100 {
			allErrs = append(allErrs, field.Invalid(p.Child(bound.name), *bound.value, "must be at least 100"))
		} else if err := validateProcessingUnits(*bound.value, p.Child(bound.name)); err != nil {
			allErrs = append(allErrs, err)
		}
		*bound.effective = *bound.value
	}
	if effMin > effMax {
		allErrs = append(allErrs, field.Invalid(p, fmt.Sprintf("min=%d, max=%d", effMin, effMax),
			"min must not be greater than max after the override is applied to the scale config"))
	}
	return allErrs
}

func validateOverrideTarget(base, t spannerv1beta1.TargetCPUUtilization, p *field.Path) (allErrs field.ErrorList) {
	if t.HighPriority == nil && t.Total == nil {
		allErrs = append(allErrs, field.Required(p, "at least one of 'highPriority' or 'total' must be specified"))
	}
	for _, target := range []struct {
		name  string
		value *int
		base  *int
	}{{"highPriority", t.HighPriority, base.HighPriority}, {"total", t.Total, base.Total}} {
		if target.value == nil {
			continue
		}
		// The syncer fetches only the metrics set in the scale config.
		if target.base == nil {
			allErrs = append(allErrs, field.Forbidden(p.Child(target.name), "can be overridden only when set in the scale config"))
		}
		if v := *target.value; v <= 0 || v >= 100 {
			allErrs = append(allErrs, field.Invalid(p.Child(target.name), v, "must be greater than 0 and less than 100"))
		}
	}
	return allErrs
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
