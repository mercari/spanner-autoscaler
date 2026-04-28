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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// log is for logging in this package.
var log = logf.Log.WithName("spannerautoscaler-resource.webhook")

func (r *SpannerAutoscaler) SetupWebhookWithManager(mgr ctrl.Manager) error {
	w := &spannerAutoscalerWebhook{}
	return ctrl.NewWebhookManagedBy(mgr, r).
		WithDefaulter(w).
		WithValidator(w).
		Complete()
}

//+kubebuilder:webhook:path=/mutate-spanner-mercari-com-v1beta1-spannerautoscaler,mutating=true,failurePolicy=fail,sideEffects=None,groups=spanner.mercari.com,resources=spannerautoscalers,verbs=create;update,versions=v1beta1,name=mspannerautoscaler.kb.io,admissionReviewVersions=v1
//+kubebuilder:webhook:path=/validate-spanner-mercari-com-v1beta1-spannerautoscaler,mutating=false,failurePolicy=fail,sideEffects=None,groups=spanner.mercari.com,resources=spannerautoscalers,verbs=create;update,versions=v1beta1,name=vspannerautoscaler.kb.io,admissionReviewVersions=v1

// spannerAutoscalerWebhook implements admission.Defaulter and admission.Validator
// for the SpannerAutoscaler resource.
type spannerAutoscalerWebhook struct{}

// Default implements admission.Defaulter so a webhook will be registered for the type.
func (*spannerAutoscalerWebhook) Default(_ context.Context, obj *SpannerAutoscaler) error {
	log.Info("default", "name", obj.Name)
	log.V(1).Info("received spannerautoscaler resource for setting defaults", "name", obj.Name, "resource", obj)

	// set the correct AuthType in Spec
	switch {
	case obj.Spec.Authentication.IAMKeySecret != nil:
		obj.Spec.Authentication.Type = AuthTypeSA
	case obj.Spec.Authentication.ImpersonateConfig != nil:
		obj.Spec.Authentication.Type = AuthTypeImpersonation
	default:
		obj.Spec.Authentication.Type = AuthTypeADC
	}

	// set appropriate namespace for secret
	if obj.Spec.Authentication.Type == AuthTypeSA && obj.Spec.Authentication.IAMKeySecret.Namespace == "" {
		obj.Spec.Authentication.IAMKeySecret.Namespace = obj.ObjectMeta.Namespace
	}

	// set default ComputeType
	if obj.Spec.ScaleConfig.ProcessingUnits != (ScaleConfigPUs{}) {
		obj.Spec.ScaleConfig.ComputeType = ComputeTypePU
	}

	// convert the nodes to processing units
	if obj.Spec.ScaleConfig.Nodes != (ScaleConfigNodes{}) {
		obj.Spec.ScaleConfig.ComputeType = ComputeTypeNode
		obj.Spec.ScaleConfig.ProcessingUnits = ScaleConfigPUs{
			Min: obj.Spec.ScaleConfig.Nodes.Min * 1000,
			Max: obj.Spec.ScaleConfig.Nodes.Max * 1000,
		}
	}

	// set default ScaledownStepSize
	if obj.Spec.ScaleConfig.ScaledownStepSize.IntValue() == 0 {
		obj.Spec.ScaleConfig.ScaledownStepSize = intstr.FromInt(2000)
	}

	log.V(1).Info("finished setting defaults for spannerautoscaler resource", "name", obj.Name, "resource", obj)
	return nil
}

// ValidateCreate implements admission.Validator so a webhook will be registered for the type.
func (*spannerAutoscalerWebhook) ValidateCreate(_ context.Context, obj *SpannerAutoscaler) (admission.Warnings, error) {
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

// ValidateUpdate implements admission.Validator so a webhook will be registered for the type.
func (*spannerAutoscalerWebhook) ValidateUpdate(_ context.Context, oldObj, newObj *SpannerAutoscaler) (admission.Warnings, error) {
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

// ValidateDelete implements admission.Validator so a webhook will be registered for the type.
func (*spannerAutoscalerWebhook) ValidateDelete(_ context.Context, obj *SpannerAutoscaler) (admission.Warnings, error) {
	log.Info("validate delete", "name", obj.Name)

	return nil, nil
}

func validateSpec(r *SpannerAutoscaler) (allErrs field.ErrorList) {
	if err := validateAuthentication(r); err != nil {
		allErrs = append(allErrs, err)
	}
	if err := validateScaleConfig(r); err != nil {
		allErrs = append(allErrs, err)
	}

	return allErrs
}

func validateAuthentication(r *SpannerAutoscaler) *field.Error {
	if r.Spec.Authentication.ImpersonateConfig != nil && r.Spec.Authentication.IAMKeySecret != nil {
		return field.Invalid(
			field.NewPath("spec").Child("authentication"),
			r.Spec.Authentication,
			"'impersonateConfig' and 'iamKeySecret' are mutually exclusive values")
	}
	return nil
}

func validateTargetCPUUtilization(cpu TargetCPUUtilization) *field.Error {
	// highPriority is required. total is optional and can be combined with highPriority
	// for dual CPU scaling (scale-out when either threshold is exceeded).
	if cpu.HighPriority == nil {
		return field.Required(
			field.NewPath("spec", "scaleConfig", "targetCPUUtilization", "highPriority"),
			"'highPriority' is required; 'total' may be specified additionally for dual CPU scaling",
		)
	}
	return nil
}

func validateScaleConfig(r *SpannerAutoscaler) *field.Error {
	sc := r.Spec.ScaleConfig

	if err := validateTargetCPUUtilization(sc.TargetCPUUtilization); err != nil {
		return err
	}

	if sc.Nodes != (ScaleConfigNodes{}) {
		if sc.Nodes.Max < sc.Nodes.Min {
			return field.Invalid(
				field.NewPath("spec").Child("scaleConfig").Child("nodes").Child("max"),
				sc.Nodes.Max,
				"'max' value must be less than 'min' value")
		}
	}

	if sc.ProcessingUnits != (ScaleConfigPUs{}) {
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
