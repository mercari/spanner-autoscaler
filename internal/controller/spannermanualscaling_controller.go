/*

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
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	utilclock "k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
)

// SpannerManualScalingReconciler owns the per-SpannerManualScaling lifecycle
// concerns that the SpannerAutoscaler reconciler cannot handle on its own:
//
//   - When the spec.targetResource does not resolve to a SpannerAutoscaler
//     in the same namespace, mark the resource phase=Invalid (terminal).
//     Without this, an orphan SpannerManualScaling would sit in Pending
//     forever — the SpannerAutoscaler reconciler only visits resources whose
//     target SA exists, and the history GC only collects terminal-phase rows.
//
//   - When the target does resolve, set an OwnerReference linking the
//     SpannerManualScaling to the target SpannerAutoscaler. This delegates
//     cascade-delete to the Kubernetes garbage collector: deleting the parent
//     SpannerAutoscaler removes all of its SpannerManualScalings.
//
// This reconciler is intentionally minimal — it does not apply the override
// to the Spanner instance itself; that is done by the SpannerAutoscaler
// reconciler which selects the newest non-terminal SpannerManualScaling per
// target. The two reconcilers cooperate via shared status writes that are
// idempotent on a per-field basis.
type SpannerManualScalingReconciler struct {
	ctrlClient ctrlclient.Client
	scheme     *runtime.Scheme
	recorder   record.EventRecorder
	clock      utilclock.Clock
	log        logr.Logger
}

// SpannerManualScalingReconcilerOption is the option-pattern interface for
// this reconciler. Mirrors the convention of SpannerAutoscalerReconciler /
// SpannerAutoscaleScheduleReconciler so cmd/main.go can pass shared options.
type SpannerManualScalingReconcilerOption interface {
	applySpannerManualScalingReconciler(r *SpannerManualScalingReconciler)
}

func (o withLog) applySpannerManualScalingReconciler(r *SpannerManualScalingReconciler) {
	r.log = o.logger.WithName("spannermanualscaling")
}

func (o withClock) applySpannerManualScalingReconciler(r *SpannerManualScalingReconciler) {
	r.clock = o.clock
}

// NewSpannerManualScalingReconciler constructs a SpannerManualScalingReconciler.
func NewSpannerManualScalingReconciler(
	ctrlClient ctrlclient.Client,
	scheme *runtime.Scheme,
	recorder record.EventRecorder,
	opts ...Option,
) *SpannerManualScalingReconciler {
	r := &SpannerManualScalingReconciler{
		ctrlClient: ctrlClient,
		scheme:     scheme,
		recorder:   recorder,
		clock:      utilclock.RealClock{},
	}
	for _, option := range opts {
		if opt, ok := option.(SpannerManualScalingReconcilerOption); ok {
			opt.applySpannerManualScalingReconciler(r)
		}
	}
	return r
}

// Reconcile implements ctrl.Reconciler.
func (r *SpannerManualScalingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.log.WithValues("manualscaling", req.NamespacedName.String())

	var ms spannerv1beta1.SpannerManualScaling
	if err := r.ctrlClient.Get(ctx, req.NamespacedName, &ms); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get SpannerManualScaling")
		return ctrl.Result{}, err
	}

	// Terminal resources are immutable from this reconciler's perspective.
	// Both Invalid (set by us) and Expired/Superseded (set by the parent SA
	// reconciler) short-circuit here.
	if isTerminalManualScalingPhase(ms.Status.Phase) {
		return ctrl.Result{}, nil
	}
	if ms.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	// Look up the target SpannerAutoscaler.
	targetKey := types.NamespacedName{Namespace: ms.Namespace, Name: ms.Spec.TargetResource}
	var target spannerv1beta1.SpannerAutoscaler
	if err := r.ctrlClient.Get(ctx, targetKey, &target); err != nil {
		if apierrors.IsNotFound(err) {
			return r.markInvalid(ctx, log, &ms, "targetResource not found")
		}
		log.Error(err, "failed to get target SpannerAutoscaler", "target", ms.Spec.TargetResource)
		return ctrl.Result{}, err
	}

	// Target exists — ensure ownerReferences point at it so cascade-delete
	// removes this resource when the parent SpannerAutoscaler is deleted.
	if !hasControllerRefTo(&ms, &target) {
		if err := controllerutil.SetControllerReference(&target, &ms, r.scheme); err != nil {
			log.Error(err, "failed to set controller reference",
				"target", ms.Spec.TargetResource)
			return ctrl.Result{}, err
		}
		if err := r.ctrlClient.Update(ctx, &ms); err != nil {
			if apierrors.IsConflict(err) {
				return ctrl.Result{Requeue: true}, nil
			}
			log.Error(err, "failed to persist owner reference",
				"target", ms.Spec.TargetResource)
			return ctrl.Result{}, err
		}
		log.Info("set controller reference to target SpannerAutoscaler",
			"target", ms.Spec.TargetResource)
	}

	// Active reconcile (applying the override) is owned by the
	// SpannerAutoscaler reconciler; our existence here is just to maintain
	// the owner-reference and Invalid-phase invariants.
	return ctrl.Result{}, nil
}

// markInvalid stamps phase=Invalid + FinishedAt + Message on the resource
// and emits an Invalid event. Idempotent: if the resource already terminal,
// returns no-op.
func (r *SpannerManualScalingReconciler) markInvalid(
	ctx context.Context,
	log logr.Logger,
	ms *spannerv1beta1.SpannerManualScaling,
	reason string,
) (ctrl.Result, error) {
	if isTerminalManualScalingPhase(ms.Status.Phase) {
		return ctrl.Result{}, nil
	}
	msg := fmt.Sprintf("%s: targetResource=%q in namespace=%q does not exist",
		reason, ms.Spec.TargetResource, ms.Namespace)
	ms.Status.Phase = spannerv1beta1.SpannerManualScalingPhaseInvalid
	ms.Status.Message = msg
	ms.Status.FinishedAt = &metav1.Time{Time: r.clock.Now()}
	if err := r.ctrlClient.Status().Update(ctx, ms); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		log.Error(err, "failed to mark SpannerManualScaling Invalid")
		return ctrl.Result{}, err
	}
	r.recorder.Event(ms, corev1.EventTypeWarning, "Invalid", msg)
	log.Info("marked SpannerManualScaling Invalid", "reason", reason)
	return ctrl.Result{}, nil
}

// SetupWithManager registers this reconciler with the controller manager.
// `For(SpannerManualScaling)` makes the reconciler the primary handler for
// every SpannerManualScaling resource; status-only writes by the
// SpannerAutoscaler reconciler are filtered out by GenerationChangedPredicate
// so we do not re-enter on every step of a stepped ramp.
func (r *SpannerManualScalingReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrlbuilder.ControllerManagedBy(mgr).
		For(&spannerv1beta1.SpannerManualScaling{}, ctrlbuilder.WithPredicates(ctrlpredicate.GenerationChangedPredicate{})).
		Complete(r)
}

// hasControllerRefTo reports whether obj already has a controller-type owner
// reference pointing at the given SpannerAutoscaler. Used to decide whether
// SetControllerReference needs to run again on subsequent reconciles.
func hasControllerRefTo(obj *spannerv1beta1.SpannerManualScaling, target *spannerv1beta1.SpannerAutoscaler) bool {
	for _, ref := range obj.OwnerReferences {
		if ref.Controller == nil || !*ref.Controller {
			continue
		}
		if ref.UID == target.UID && ref.Kind == "SpannerAutoscaler" {
			return true
		}
	}
	return false
}
