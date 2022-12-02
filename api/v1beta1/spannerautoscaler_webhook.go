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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// log is for logging in this package.
var log = logf.Log.WithName("spannerautoscaler-resource.webhook")

func (r *SpannerAutoscaler) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		Complete()
}

//+kubebuilder:webhook:path=/mutate-spanner-mercari-com-v1beta1-spannerautoscaler,mutating=true,failurePolicy=fail,sideEffects=None,groups=spanner.mercari.com,resources=spannerautoscalers,verbs=create;update,versions=v1beta1,name=mspannerautoscaler.kb.io,admissionReviewVersions=v1

var _ webhook.Defaulter = &SpannerAutoscaler{}

// Default implements webhook.Defaulter so a webhook will be registered for the type
func (r *SpannerAutoscaler) Default() {
	log.Info("default", "name", r.Name)
	log.V(1).Info("received spannerautoscaler resource for setting defaults", "name", r.Name, "resource", r)

	// set the correct AuthType in Spec
	switch {
	case r.Spec.Authentication.IAMKeySecret != nil:
		r.Spec.Authentication.Type = AuthTypeSA
	case r.Spec.Authentication.ImpersonateConfig != nil:
		r.Spec.Authentication.Type = AuthTypeImpersonation
	default:
		r.Spec.Authentication.Type = AuthTypeADC
	}

	// set appropriate namespace for secret
	if r.Spec.Authentication.Type == AuthTypeSA && r.Spec.Authentication.IAMKeySecret.Namespace == "" {
		r.Spec.Authentication.IAMKeySecret.Namespace = r.ObjectMeta.Namespace
	}

	// set default ComputeType
	if r.Spec.ScaleConfig.ProcessingUnits != (ScaleConfigPUs{}) {
		r.Spec.ScaleConfig.ComputeType = ComputeTypePU
	}

	// convert the nodes to processing units
	if r.Spec.ScaleConfig.Nodes != (ScaleConfigNodes{}) {
		r.Spec.ScaleConfig.ComputeType = ComputeTypeNode
		r.Spec.ScaleConfig.ProcessingUnits = ScaleConfigPUs{
			Min: r.Spec.ScaleConfig.Nodes.Min * 1000,
			Max: r.Spec.ScaleConfig.Nodes.Max * 1000,
		}
	}

	// set default ScaledownStepSize
	if r.Spec.ScaleConfig.ScaledownStepSize == 0 {
		r.Spec.ScaleConfig.ScaledownStepSize = 2000
	}

	log.V(1).Info("finished setting defaults for spannerautoscaler resource", "name", r.Name, "resource", r)
}

//+kubebuilder:webhook:path=/validate-spanner-mercari-com-v1beta1-spannerautoscaler,mutating=false,failurePolicy=fail,sideEffects=None,groups=spanner.mercari.com,resources=spannerautoscalers,verbs=create;update,versions=v1beta1,name=vspannerautoscaler.kb.io,admissionReviewVersions=v1

var _ webhook.Validator = &SpannerAutoscaler{}

// ValidateCreate implements webhook.Validator so a webhook will be registered for the type
func (r *SpannerAutoscaler) ValidateCreate() error {
	log.Info("validate create", "name", r.Name)
	log.V(1).Info("validating creation of SpannerAutoscaler resource", "name", r.Name, "resource", r)

	allErrs := r.validateSpec()

	if len(allErrs) == 0 {
		return nil
	}

	return apierrors.NewInvalid(
		schema.GroupKind{Group: "spanner.mercari.com", Kind: "SpannerAutoscaler"},
		r.Name, allErrs)
}

// ValidateUpdate implements webhook.Validator so a webhook will be registered for the type
func (r *SpannerAutoscaler) ValidateUpdate(old runtime.Object) error {
	log.Info("validate update", "name", r.Name)
	log.V(1).Info("validating updates to SpannerAutoscaler resource", "name", r.Name, "resource", r)

	var allErrs field.ErrorList
	oldResource := old.(*SpannerAutoscaler)

	if oldResource.Spec.TargetInstance != r.Spec.TargetInstance {
		err := field.Invalid(
			field.NewPath("spec").Child("targetInstance"),
			r.Spec.TargetInstance,
			"'targetInstance' is not allowed to be changed after resource has been created")
		allErrs = append(allErrs, err)
	}

	allErrs = append(allErrs, r.validateSpec()...)

	if len(allErrs) == 0 {
		return nil
	}

	return apierrors.NewInvalid(
		schema.GroupKind{Group: "spanner.mercari.com", Kind: "SpannerAutoscaler"},
		r.Name, allErrs)
}

// ValidateDelete implements webhook.Validator so a webhook will be registered for the type
func (r *SpannerAutoscaler) ValidateDelete() error {
	log.Info("validate delete", "name", r.Name)

	// TODO(user): fill in your validation logic upon object deletion.
	return nil
}

func (r *SpannerAutoscaler) validateSpec() (allErrs field.ErrorList) {
	if err := r.validateAuthentication(); err != nil {
		allErrs = append(allErrs, err)
	}
	if err := r.validateScaleConfig(); err != nil {
		allErrs = append(allErrs, err)
	}

	return allErrs
}

func (r *SpannerAutoscaler) validateAuthentication() *field.Error {
	if r.Spec.Authentication.ImpersonateConfig != nil && r.Spec.Authentication.IAMKeySecret != nil {
		return field.Invalid(
			field.NewPath("spec").Child("authentication"),
			r.Spec.Authentication,
			"'impersonateConfig' and 'iamKeySecret' are mutually exclusive values")
	}
	return nil
}

func (r *SpannerAutoscaler) validateScaleConfig() *field.Error {
	sc := r.Spec.ScaleConfig

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

	if sc.ScaledownStepSize > 1000 && sc.ScaledownStepSize%1000 != 0 {
		return field.Invalid(
			field.NewPath("spec").Child("scaleConfig").Child("scaledownStepSize"),
			sc.ScaledownStepSize,
			"must be a multiple of 1000 for values which are greater than 1000")
	} else if sc.ScaledownStepSize < 1000 && sc.ScaledownStepSize%100 != 0 {
		return field.Invalid(
			field.NewPath("spec").Child("scaleConfig").Child("scaledownStepSize"),
			sc.ScaledownStepSize,
			"must be a multiple of 100 for values which are less than 1000")
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
