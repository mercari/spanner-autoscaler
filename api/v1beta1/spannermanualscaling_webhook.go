/*
Copyright 2026.

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
	"errors"
	"fmt"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var spannermanualscalinglog = logf.Log.WithName("spannermanualscaling-resource.webhook")

// errNilOldObject is returned by ValidateUpdate when the kube-apiserver
// invokes the webhook without an oldObj. This should never happen on
// UPDATE — Kubernetes always passes both objects — but the guard exists
// because reflect.DeepEqual on a nil dereference would otherwise panic.
// Exported via errors.Is to keep test assertions decoupled from the
// concrete message string.
var errNilOldObject = errors.New("spannermanualscaling webhook: oldObj is nil on update")

// ManualScalingWebhookOption configures the SpannerManualScaling validating
// webhook. Callers (typically cmd/main.go) wire up controller flags such as
// --reject-manual-scaledown via these options so the validation behavior
// reflects the cluster-wide policy.
//
// +kubebuilder:object:generate=false
type ManualScalingWebhookOption func(*spannerManualScalingWebhook)

// WithRejectManualScaledown enables the cluster-wide policy that rejects any
// SpannerManualScaling whose spec.processingUnits would reduce the target
// SpannerAutoscaler's currently observed processing units.
func WithRejectManualScaledown(enabled bool) ManualScalingWebhookOption {
	return func(w *spannerManualScalingWebhook) { w.rejectScaledown = enabled }
}

// SetupWebhookWithManager registers the validating webhook for
// SpannerManualScaling. The ManualScalingWebhookOption values let the caller
// inject the cluster-wide policy flags so the webhook's behavior matches the
// controller's reconcile-time enforcement.
func (r *SpannerManualScaling) SetupWebhookWithManager(mgr ctrl.Manager, opts ...ManualScalingWebhookOption) error {
	w := &spannerManualScalingWebhook{
		client: mgr.GetClient(),
	}
	for _, opt := range opts {
		opt(w)
	}
	return ctrl.NewWebhookManagedBy(mgr, r).
		WithValidator(w).
		Complete()
}

//+kubebuilder:webhook:path=/validate-spanner-mercari-com-v1beta1-spannermanualscaling,mutating=false,failurePolicy=fail,sideEffects=None,groups=spanner.mercari.com,resources=spannermanualscalings,verbs=create;update,versions=v1beta1,name=vspannermanualscaling.kb.io,admissionReviewVersions=v1

type spannerManualScalingWebhook struct {
	// client is used for cross-resource lookups (target SpannerAutoscaler for
	// reject-scaledown enforcement; sibling SpannerManualScalings for the
	// duplicate-create admission warning).
	client ctrlclient.Client

	// rejectScaledown mirrors the --reject-manual-scaledown controller flag.
	rejectScaledown bool
}

// ValidateCreate enforces the create-time invariants on SpannerManualScaling:
//   - basic field shape (target name, processing-units validity, ramp field
//     shape, expiresAt in the future)
//   - cluster-wide --reject-manual-scaledown policy (cross-resource lookup of
//     the target's currentProcessingUnits; fail-closed on lookup failure)
//   - admission warnings for typo-likely patterns (interval-only ramp spec,
//     duplicate active SpannerManualScaling on the same target)
//
// Spec immutability is enforced in ValidateUpdate, not here.
func (w *spannerManualScalingWebhook) ValidateCreate(ctx context.Context, obj *SpannerManualScaling) (admission.Warnings, error) {
	spannermanualscalinglog.Info("validate create", "name", obj.Name)

	allErrs := validateManualScalingSpec(&obj.Spec, true /* isCreate */)

	// Reject-scaledown is a hard policy: lookup must succeed and the new
	// target PU must not be lower than the parent's current PU. Failure to
	// look up the parent is fail-closed (we cannot prove the policy is
	// satisfied, so we refuse). This matches the controller's reconcile-time
	// defense in depth and ensures a webhook bypass (--validate=false) cannot
	// smuggle a scaledown through.
	if w.rejectScaledown {
		if err := w.checkRejectScaledown(ctx, obj); err != nil {
			allErrs = append(allErrs, err)
		}
	}

	if len(allErrs) > 0 {
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: "spanner.mercari.com", Kind: "SpannerManualScaling"},
			obj.Name, allErrs)
	}

	// Warnings: surface typo / duplicate patterns to the operator but don't
	// block create. The newest-wins rule handles the duplicate case
	// correctly at runtime; we just want the operator to notice.
	var warnings admission.Warnings
	warnings = append(warnings, intervalWithoutStepSizeWarnings(&obj.Spec)...)
	warnings = append(warnings, w.duplicateActiveWarning(ctx, obj)...)
	return warnings, nil
}

// ValidateUpdate enforces spec immutability. The "change via newest-wins
// create" pattern atomically swaps
// active overrides without an autoscaling gap, so there is no need to support
// in-place spec mutation — and disallowing it removes a class of
// "scale-up admitted, scaledown smuggled via update" attack vectors.
func (w *spannerManualScalingWebhook) ValidateUpdate(_ context.Context, oldObj, newObj *SpannerManualScaling) (admission.Warnings, error) {
	spannermanualscalinglog.Info("validate update", "name", newObj.Name)

	if oldObj == nil {
		return nil, errNilOldObject
	}

	if !reflect.DeepEqual(oldObj.Spec, newObj.Spec) {
		fldErr := field.Forbidden(
			field.NewPath("spec"),
			"spec is immutable; create a new SpannerManualScaling instead. "+
				"The newest creationTimestamp becomes Active and supersedes the older resource "+
				"atomically (no autoscaling gap). To force expiration, kubectl delete this resource.")
		return nil, apierrors.NewInvalid(
			schema.GroupKind{Group: "spanner.mercari.com", Kind: "SpannerManualScaling"},
			newObj.Name, field.ErrorList{fldErr})
	}

	return nil, nil
}

// ValidateDelete is a no-op. Deletion is the operator-visible mechanism for
// force-expiring an active override; no validation needed.
func (w *spannerManualScalingWebhook) ValidateDelete(_ context.Context, obj *SpannerManualScaling) (admission.Warnings, error) {
	spannermanualscalinglog.Info("validate delete", "name", obj.Name)
	return nil, nil
}

// validateManualScalingSpec checks field-shape invariants that do not require
// cross-resource lookups. isCreate gates the "expiresAt must be in the
// future" check, which only applies at create time.
func validateManualScalingSpec(spec *SpannerManualScalingSpec, isCreate bool) field.ErrorList {
	var allErrs field.ErrorList

	if spec.TargetResource == "" {
		allErrs = append(allErrs, field.Required(
			field.NewPath("spec").Child("targetResource"),
			"targetResource must reference a SpannerAutoscaler in the same namespace"))
	}

	if spec.ProcessingUnits <= 0 {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec").Child("processingUnits"),
			spec.ProcessingUnits,
			"processingUnits must be a positive multiple of 100 (when <1000) or 1000"))
	} else if spec.ProcessingUnits < 1000 && spec.ProcessingUnits%100 != 0 {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec").Child("processingUnits"),
			spec.ProcessingUnits,
			"processingUnits below 1000 must be a multiple of 100"))
	} else if spec.ProcessingUnits >= 1000 && spec.ProcessingUnits%1000 != 0 {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec").Child("processingUnits"),
			spec.ProcessingUnits,
			"processingUnits at or above 1000 must be a multiple of 1000"))
	}

	if e := validateStepSize(spec.ScaleupStepSize, field.NewPath("spec").Child("scaleupStepSize")); e != nil {
		allErrs = append(allErrs, e)
	}
	if e := validateStepSize(spec.ScaledownStepSize, field.NewPath("spec").Child("scaledownStepSize")); e != nil {
		allErrs = append(allErrs, e)
	}

	if e := validatePositiveDuration(spec.ScaleupInterval, field.NewPath("spec").Child("scaleupInterval")); e != nil {
		allErrs = append(allErrs, e)
	}
	if e := validatePositiveDuration(spec.ScaledownInterval, field.NewPath("spec").Child("scaledownInterval")); e != nil {
		allErrs = append(allErrs, e)
	}

	if isCreate && spec.ExpiresAt != nil && !spec.ExpiresAt.After(time.Now()) {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec").Child("expiresAt"),
			spec.ExpiresAt.Time,
			"expiresAt must be in the future at create time"))
	}

	return allErrs
}

// validateStepSize accepts the same int-or-percent shapes the parent
// SpannerAutoscaler's scaleConfig step size fields accept:
//   - integer values: positive multiple of 100 (when <1000) or 1000
//   - percentage values: 1%-100%
//
// nil is allowed (means "not set").
func validateStepSize(s *intstr.IntOrString, fldPath *field.Path) *field.Error {
	if s == nil {
		return nil
	}
	switch s.Type {
	case intstr.Int:
		v := int(s.IntVal)
		if v <= 0 {
			return field.Invalid(fldPath, v, "step size must be positive")
		}
		if v < 1000 && v%100 != 0 {
			return field.Invalid(fldPath, v, "step size below 1000 must be a multiple of 100")
		}
		if v >= 1000 && v%1000 != 0 {
			return field.Invalid(fldPath, v, "step size at or above 1000 must be a multiple of 1000")
		}
		return nil
	case intstr.String:
		// Defer to the upstream parser used by the runtime path; it accepts
		// the "N%" form and we then range-check N. basePU=10000 makes the
		// parsed result equal N*100 (so the [100, 10000] window means
		// 1%–100%, rejecting "0%" and ">100%").
		v, err := intstr.GetScaledValueFromIntOrPercent(s, 10000, false)
		if err != nil {
			return field.Invalid(fldPath, s.StrVal, fmt.Sprintf("invalid percentage value: %v", err))
		}
		if v < 100 || v > 10000 {
			return field.Invalid(fldPath, s.StrVal, "percentage step size must be between 1% and 100%")
		}
		return nil
	default:
		return field.Invalid(fldPath, s.String(), "step size has an unknown intstr type")
	}
}

// validatePositiveDuration rejects zero and negative durations on the
// scaleup/scaledown interval fields. A nil pointer (field unset) is allowed
// and falls back at runtime to the controller's --scale-up-interval /
// --scale-down-interval flag default.
func validatePositiveDuration(d *metav1.Duration, fldPath *field.Path) *field.Error {
	if d == nil {
		return nil
	}
	if d.Duration <= 0 {
		return field.Invalid(fldPath, d.Duration.String(), "interval must be a positive duration")
	}
	return nil
}

// intervalWithoutStepSizeWarnings flags the typo pattern where an operator
// sets an interval field but forgets the corresponding step size. At runtime
// such an override is treated as single-jump (the interval is ignored), so we
// surface an admission warning rather than block create — the value is still
// inert, just unexpectedly so.
func intervalWithoutStepSizeWarnings(spec *SpannerManualScalingSpec) admission.Warnings {
	var w admission.Warnings
	if spec.ScaleupInterval != nil && spec.ScaleupStepSize == nil {
		w = append(w,
			"spec.scaleupInterval is set but spec.scaleupStepSize is not. "+
				"Without a step size, this override is single-jump scale-up and the interval has no effect. "+
				"If you intended a stepped ramp, also set spec.scaleupStepSize.")
	}
	if spec.ScaledownInterval != nil && spec.ScaledownStepSize == nil {
		w = append(w,
			"spec.scaledownInterval is set but spec.scaledownStepSize is not. "+
				"Without a step size, this override is single-jump scale-down and the interval has no effect. "+
				"If you intended a stepped ramp, also set spec.scaledownStepSize.")
	}
	return w
}

// duplicateActiveWarning lists same-namespace SpannerManualScalings targeting
// the same SpannerAutoscaler and, if any non-terminal one already exists,
// returns a warning naming it. The duplicate is *not* rejected — the
// newest-wins rule handles the race correctly at runtime, and atomic
// swap of overrides is a desired property — but the warning surfaces typos
// (running the same create twice) and dual-on-call accidents.
//
// List failures are fail-open (no warning emitted) because the runtime
// handles the duplicate correctly regardless.
func (w *spannerManualScalingWebhook) duplicateActiveWarning(ctx context.Context, obj *SpannerManualScaling) admission.Warnings {
	if w.client == nil {
		return nil
	}
	var list SpannerManualScalingList
	if err := w.client.List(ctx, &list, ctrlclient.InNamespace(obj.Namespace)); err != nil {
		spannermanualscalinglog.V(1).Info("duplicate-active lookup failed; skipping warning",
			"namespace", obj.Namespace, "err", err)
		return nil
	}
	for i := range list.Items {
		other := &list.Items[i]
		if other.Spec.TargetResource != obj.Spec.TargetResource {
			continue
		}
		if other.Name == obj.Name {
			// On create the new object is not yet in etcd, but generated
			// names can collide with sibling listings during tests.
			continue
		}
		if other.DeletionTimestamp != nil {
			continue
		}
		if other.Status.Phase.IsTerminal() {
			continue
		}
		return admission.Warnings{
			fmt.Sprintf(
				"A SpannerManualScaling for targetResource=%q already exists "+
					"(name=%q, phase=%s). Creating this resource will supersede it "+
					"(the older resource transitions to phase=Superseded). "+
					"If unintended, delete this new resource and inspect the existing one first.",
				obj.Spec.TargetResource, other.Name, other.Status.Phase),
		}
	}
	return nil
}

// checkRejectScaledown enforces the --reject-manual-scaledown policy at
// admission time. It looks up the target SpannerAutoscaler in the same
// namespace and rejects the create if the new spec.processingUnits is below
// the parent's currently observed processingUnits.
//
// Fail-closed on lookup failure: if the target cannot be retrieved (NotFound
// or transient API error) we cannot prove the policy is satisfied, so we
// reject. This is the safe direction for risk-management opt-in.
//
// The check is skipped when the parent's CurrentProcessingUnits is 0 — in
// that case the autoscaler has not yet synced, there is no "current" value
// to compare against, and we let the create through so operators are not
// blocked on freshly-created autoscalers.
func (w *spannerManualScalingWebhook) checkRejectScaledown(ctx context.Context, obj *SpannerManualScaling) *field.Error {
	if w.client == nil {
		return field.Forbidden(
			field.NewPath("spec").Child("processingUnits"),
			"cluster policy --reject-manual-scaledown=true requires cross-resource lookup, "+
				"but the webhook has no client configured")
	}
	var target SpannerAutoscaler
	key := types.NamespacedName{Namespace: obj.Namespace, Name: obj.Spec.TargetResource}
	if err := w.client.Get(ctx, key, &target); err != nil {
		return field.Forbidden(
			field.NewPath("spec").Child("processingUnits"),
			fmt.Sprintf(
				"cluster policy --reject-manual-scaledown=true requires looking up the target "+
					"SpannerAutoscaler %q, but the lookup failed: %v. Fail-closed (rejected).",
				obj.Spec.TargetResource, err))
	}
	current := target.Status.CurrentProcessingUnits
	if current == 0 {
		// Target hasn't reported its current PU yet (newly created
		// autoscaler not yet synced). No "current" to compare against —
		// allow.
		return nil
	}
	if obj.Spec.ProcessingUnits < current {
		return field.Forbidden(
			field.NewPath("spec").Child("processingUnits"),
			fmt.Sprintf(
				"spec.processingUnits=%d would reduce target SpannerAutoscaler %q from "+
					"currentProcessingUnits=%d. Cluster policy --reject-manual-scaledown=true "+
					"forbids scaledown via SpannerManualScaling.",
				obj.Spec.ProcessingUnits, obj.Spec.TargetResource, current))
	}
	return nil
}
