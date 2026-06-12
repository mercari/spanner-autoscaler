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
	"sort"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	"github.com/mercari/spanner-autoscaler/internal/observability"
	syncerpkg "github.com/mercari/spanner-autoscaler/internal/syncer"
)

// dispatchManualScaling runs the manual-scaling reconcile and converts its
// (handled, requeueAfter, err) return into the ctrlreconcile.Result shape
// the caller returns. On the success path it persists sa.Status, maps
// status-update conflicts to a benign requeue, and emits the per-entry
// schedule-deactivation events that the caller queued before invoking this
// function. handled=true tells the caller to short-circuit Reconcile;
// handled=false means there was no active override and Reconcile should
// fall through to the CPU autoscale path (which emits the deactivations
// itself).
func (r *SpannerAutoscalerReconciler) dispatchManualScaling(
	ctx context.Context,
	log logr.Logger,
	sa *spannerv1beta1.SpannerAutoscaler,
	syncer syncerpkg.Syncer,
	statusChanged *bool,
	droppedActiveSchedules []spannerv1beta1.ActiveSchedule,
) (ctrlreconcile.Result, bool, error) {
	handled, requeueAfter, err := r.reconcileManualScaling(ctx, log, sa, syncer, r.clock.Now(), statusChanged)
	if err != nil {
		if *statusChanged {
			sa.Status.DesiredProcessingUnits = sa.Status.CurrentProcessingUnits
			if updateErr := r.ctrlClient.Status().Update(ctx, sa); updateErr != nil && !apierrors.IsConflict(updateErr) {
				log.Error(updateErr, "manual scaling: failed to persist status after error")
			}
		}
		return ctrlreconcile.Result{}, true, err
	}
	if !handled {
		return ctrlreconcile.Result{}, false, nil
	}
	if *statusChanged {
		if uerr := r.ctrlClient.Status().Update(ctx, sa); uerr != nil {
			if apierrors.IsConflict(uerr) {
				// Benign: another writer beat us; requeue with the latest version.
				return ctrlreconcile.Result{Requeue: true}, true, nil
			}
			r.recorder.Event(sa, corev1.EventTypeWarning, "FailedUpdateStatus", uerr.Error())
			log.Error(uerr, "manual scaling: failed to update spanner autoscaler status")
			return ctrlreconcile.Result{}, true, uerr
		}
		// Status persisted — emit schedule deactivations that were pruned in
		// the same reconcile (analog of the trailing block in Reconcile's CPU
		// path).
		emitScheduleDeactivations(log, sa, droppedActiveSchedules)
	}
	return ctrlreconcile.Result{RequeueAfter: requeueAfter}, true, nil
}

// emitScheduleDeactivations logs and increments the deactivation counter for
// every entry in droppedActiveSchedules. Called from both the manual scaling
// success path (dispatchManualScaling) and the CPU autoscale success path
// (Reconcile's trailing block) after sa.Status has been persisted, so a
// failed Status().Update does not lose the event and a successful persist
// emits it exactly once.
func emitScheduleDeactivations(log logr.Logger, sa *spannerv1beta1.SpannerAutoscaler, dropped []spannerv1beta1.ActiveSchedule) {
	if len(dropped) == 0 {
		return
	}
	labels := observability.LabelsForAutoscaler(sa)
	for _, as := range dropped {
		log.Info("removed currently active schedule",
			"schedule", as.ScheduleName,
			"additionalPU", as.AdditionalPU,
			"endTime", as.EndTime.Time,
		)
		observability.RecordScheduleDeactivation(labels, observability.ScheduleDeactivationUnregistered)
	}
}

// reconcileManualScaling drives any active SpannerManualScaling that targets
// the given SpannerAutoscaler. It is called from Reconcile after the
// InstanceState/CurrentProcessingUnits guards and before the CPU-based
// desired PU calculation.
//
// Return values:
//   - handled: true when a manual override was Active (or held due to
//     cooldown) and Reconcile should return immediately rather than running
//     the CPU path. The caller honors requeueAfter.
//   - requeueAfter: when the next reconcile should fire. The earlier of the
//     next ramp step boundary and ExpiresAt; 0 means "no requested wakeup,
//     rely on watches".
//   - err: any error from cross-resource list/Get, syncer.UpdateInstance, or
//     status writes. Conflict errors are surfaced to controller-runtime so it
//     can requeue on the latest resourceVersion.
//
// statusChanged is set when this function mutates sa.Status so the caller
// persists the change.
func (r *SpannerAutoscalerReconciler) reconcileManualScaling(
	ctx context.Context,
	log logr.Logger,
	sa *spannerv1beta1.SpannerAutoscaler,
	syncer syncerpkg.Syncer,
	now time.Time,
	statusChanged *bool,
) (handled bool, requeueAfter time.Duration, err error) {
	active, superseded, err := r.selectActiveManualScaling(ctx, sa, now)
	if err != nil {
		log.Error(err, "manual scaling: list failed")
		return false, 0, err
	}

	// Mark every superseded sibling. This runs whether or not we ultimately
	// apply the winner, so display state stays consistent even if we end up
	// rejecting the active one.
	for i := range superseded {
		if perr := r.transitionManualScalingPhase(ctx, &superseded[i],
			spannerv1beta1.SpannerManualScalingPhaseSuperseded,
			fmt.Sprintf("superseded by newer SpannerManualScaling targeting %q",
				superseded[i].Spec.TargetResource),
			now,
		); perr != nil {
			log.Error(perr, "manual scaling: failed to mark superseded", "name", superseded[i].Name)
			// Continue: a single phase write failure should not block the
			// active path.
		}
	}

	// Also sweep any siblings whose ExpiresAt elapsed but the controller
	// never observed them (e.g. controller was down across the boundary).
	// The Invalid phase for orphan targetResource is owned by the
	// SpannerManualScalingReconciler — see spannermanualscaling_controller.go.
	r.markExpiredSiblings(ctx, log, sa, now)

	if active == nil {
		if sa.Status.ActiveManualScaling != nil {
			r.recorder.Eventf(sa, corev1.EventTypeNormal, "ManualScalingDeactivated",
				"manual scaling deactivated (no active SpannerManualScaling for this target)")
		}
		r.clearActiveManualScalingState(sa, statusChanged)
		return false, 0, nil
	}

	// We have an Active candidate. Defense-in-depth scaledown check.
	if r.rejectManualScaledown && active.Spec.ProcessingUnits < sa.Status.CurrentProcessingUnits {
		r.rejectScaledownPolicy(ctx, log, sa, active, now, statusChanged)
		return false, 0, nil
	}

	// We will apply the override. Suppress schedule-derived min/max so
	// DesiredProcessingUnits is bounded by the manual target only.
	if sa.Status.DesiredMinPUs != 0 || sa.Status.DesiredMaxPUs != 0 {
		sa.Status.DesiredMinPUs = 0
		sa.Status.DesiredMaxPUs = 0
		*statusChanged = true
	}

	// When schedules are in their time window at the same time as this
	// override, the override wins (its target replaces the schedule-bumped
	// range computed on the CPU path). Surface the preempt to operators so
	// that "schedule is configured but PU does not reflect its
	// additionalPU" is not mistaken for a bug, and so it shows up in the
	// scale_skipped metric like other not-applied reasons.
	if scheds := sa.Status.CurrentlyActiveSchedules; len(scheds) > 0 {
		names := make([]string, 0, len(scheds))
		for _, as := range scheds {
			names = append(names, as.ScheduleName)
		}
		log.Info("manual scaling preempting active schedule(s)",
			"source", active.Name,
			"schedules", names,
		)
		observability.RecordScaleSkipped(
			observability.LabelsForAutoscaler(sa),
			observability.SkipReasonScheduleSuppressedByManual,
		)
	}

	// ramp reflects what this reconcile will actually do (stepped vs
	// single-jump for the direction being driven), not just whether any step
	// size is set in the spec. See manualScalingActiveRamp's docstring.
	ramp := manualScalingActiveRamp(&active.Spec, sa.Status.CurrentProcessingUnits)
	targetPU := active.Spec.ProcessingUnits
	nextPU, nextInterval := r.nextManualPU(sa, active, now)

	// Update SpannerAutoscaler.status.activeManualScaling before any
	// instance change, so the snapshot reflects "we attempted to apply
	// this override" even if the API call below fails.
	expiresAtCopy := active.Spec.ExpiresAt.DeepCopy()
	desiredActive := &spannerv1beta1.ActiveManualScaling{
		Name:            active.Name,
		ProcessingUnits: targetPU,
		Ramp:            ramp,
		ExpiresAt:       expiresAtCopy,
	}
	if !activeManualScalingEqual(sa.Status.ActiveManualScaling, desiredActive) {
		sa.Status.ActiveManualScaling = desiredActive
		*statusChanged = true
	}

	didApply := false
	if nextPU != sa.Status.CurrentProcessingUnits {
		labels := observability.LabelsForAutoscaler(sa)
		updateStart := r.clock.Now()
		updateErr := syncer.UpdateInstance(ctx, nextPU)
		observability.RecordInstanceUpdate(labels, r.clock.Now().Sub(updateStart), updateErr)
		if updateErr != nil {
			r.recorder.Event(sa, corev1.EventTypeWarning, "FailedUpdateInstance", updateErr.Error())
			log.Error(updateErr, "manual scaling: failed to update spanner instance",
				"source", active.Name, "nextPU", nextPU)
			return false, 0, updateErr
		}
		observability.RecordScaleEvent(labels,
			sa.Status.CurrentProcessingUnits, nextPU,
			observability.DriverForManualScaling(ramp),
		)
		sa.Status.LastScaleTime = metav1.Time{Time: now}
		sa.Status.DesiredProcessingUnits = nextPU
		*statusChanged = true
		didApply = true

		if nextPU != targetPU {
			r.recorder.Eventf(sa, corev1.EventTypeNormal, "ManualScalingProgressing",
				"manual scaling step: %d -> %d (target=%d, source=%s)",
				sa.Status.CurrentProcessingUnits, nextPU, targetPU, active.Name)
		} else {
			r.recorder.Eventf(sa, corev1.EventTypeNormal, "ManualScalingApplied",
				"manual scaling applied: pinned PU to %d (source=%s)",
				nextPU, active.Name)
		}
		log.Info("manual scaling: applied",
			"source", active.Name, "before", sa.Status.CurrentProcessingUnits,
			"after", nextPU, "target", targetPU, "ramp", ramp,
		)
	} else {
		// nextPU == currentPU. Distinguish "we are at target" from "we are
		// holding because the override's cooldown has not elapsed yet" — the
		// latter is a cooldown-bounded skip, not a no-op. nextInterval > 0
		// here means nextManualPU returned the current PU AND scheduled a
		// follow-up wake-up at the cooldown boundary; we are mid-ramp,
		// waiting.
		reason := observability.SkipReasonSame
		if nextInterval > 0 && sa.Status.CurrentProcessingUnits != targetPU {
			if active.Spec.ProcessingUnits > sa.Status.CurrentProcessingUnits {
				reason = observability.SkipReasonScaleUpInterval
			} else {
				reason = observability.SkipReasonScaleDownInterval
			}
		}
		observability.RecordScaleSkipped(observability.LabelsForAutoscaler(sa), reason)
	}

	// Update the SpannerManualScaling status (phase / progress fields).
	// Pass didApply so AppliedAt is only stamped on a reconcile that actually
	// invoked syncer.UpdateInstance — otherwise the timestamp drifts onto
	// no-op reconciles (cooldown hold) and the docstring contract
	// ("the time the controller first applied this override") would lie.
	if perr := r.updateManualScalingProgress(ctx, active, sa.Status.CurrentProcessingUnits, targetPU, now, didApply); perr != nil {
		log.Error(perr, "manual scaling: failed to update SpannerManualScaling status", "name", active.Name)
		// Don't fail the reconcile on a status write conflict; the next
		// reconcile will retry.
	}

	// Emit / update manual_scaling_* gauges.
	identityLabels := observability.LabelsForAutoscaler(sa)
	observability.RecordManualScalingActive(identityLabels, true, ramp)
	observability.RecordManualScalingTarget(identityLabels, targetPU, ramp)

	// RequeueAfter: pick the earlier of next-step boundary and ExpiresAt.
	requeueAfter = nextInterval
	if active.Spec.ExpiresAt != nil {
		d := active.Spec.ExpiresAt.Time.Sub(now)
		if d < 0 {
			d = time.Millisecond // already expired; wake up immediately
		}
		if requeueAfter == 0 || d < requeueAfter {
			requeueAfter = d
		}
	}
	return true, requeueAfter, nil
}

// selectActiveManualScaling lists SpannerManualScaling resources in the same
// namespace as sa and picks the one (if any) that should drive the
// autoscaler right now. Selection rules:
//
//   - same targetResource as sa.Name
//   - DeletionTimestamp not set
//   - ExpiresAt either nil or in the future
//   - not already in a terminal phase (Expired, Superseded, Invalid)
//
// Among multiple candidates, the newest creationTimestamp wins. Ties (same
// second, common under scripted bulk creates) are broken by name DESC to
// keep selection deterministic.
//
// Returns the active candidate (or nil if none) and the slice of siblings
// that should be marked Superseded.
func (r *SpannerAutoscalerReconciler) selectActiveManualScaling(
	ctx context.Context,
	sa *spannerv1beta1.SpannerAutoscaler,
	now time.Time,
) (*spannerv1beta1.SpannerManualScaling, []spannerv1beta1.SpannerManualScaling, error) {
	var list spannerv1beta1.SpannerManualScalingList
	if err := r.ctrlClient.List(ctx, &list, ctrlclient.InNamespace(sa.Namespace)); err != nil {
		return nil, nil, err
	}
	var candidates []spannerv1beta1.SpannerManualScaling
	for i := range list.Items {
		ms := &list.Items[i]
		if ms.Spec.TargetResource != sa.Name {
			continue
		}
		if ms.DeletionTimestamp != nil {
			continue
		}
		if ms.Spec.ExpiresAt != nil && !now.Before(ms.Spec.ExpiresAt.Time) {
			continue
		}
		if ms.Status.Phase.IsTerminal() {
			continue
		}
		candidates = append(candidates, *ms)
	}
	if len(candidates) == 0 {
		return nil, nil, nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		ti := candidates[i].CreationTimestamp.Time
		tj := candidates[j].CreationTimestamp.Time
		if !ti.Equal(tj) {
			return ti.After(tj)
		}
		// Tie-breaker for same-second creations (creationTimestamp is
		// 1-second granularity in k8s; scripted bulk creates can collide):
		// name DESC keeps selection deterministic.
		return candidates[i].Name > candidates[j].Name
	})
	return &candidates[0], candidates[1:], nil
}

// nextManualPU returns the PU to apply on this reconcile and, when non-zero,
// the duration after which another reconcile should fire so the next step
// can be applied (used as RequeueAfter for stepped ramps).
//
// The step-size field for the required direction is the sole signal that
// gates stepped vs single-jump. The interval field is consulted only when
// the step size is set; when interval is unset, the controller's
// scaleUpInterval / scaleDownInterval flag default is used (the same
// default that governs CPU-driven cadence on SpannerAutoscaler).
//
// Logic is delegated to nextRampPU so SpannerManualScaling and a future
// SpannerAutoscaleSchedule ramp share identical step / interval / cooldown
// semantics; this method only adapts spec fields into rampStep values.
func (r *SpannerAutoscalerReconciler) nextManualPU(
	sa *spannerv1beta1.SpannerAutoscaler,
	ms *spannerv1beta1.SpannerManualScaling,
	now time.Time,
) (int, time.Duration) {
	up := rampStep{
		StepSize:        ms.Spec.ScaleupStepSize,
		Interval:        ms.Spec.ScaleupInterval,
		DefaultInterval: r.scaleUpInterval,
	}
	down := rampStep{
		StepSize:        ms.Spec.ScaledownStepSize,
		Interval:        ms.Spec.ScaledownInterval,
		DefaultInterval: r.scaleDownInterval,
	}
	return nextRampPU(
		sa.Status.CurrentProcessingUnits,
		ms.Spec.ProcessingUnits,
		up, down,
		sa.Status.LastScaleTime.Time,
		now,
	)
}

// updateManualScalingProgress writes phase / progress fields onto the active
// SpannerManualScaling. It transitions phase to Active or Progressing
// depending on whether the target has been reached, and stamps AppliedAt /
// ReachedAt the first time each happens.
//
// didApply tells the function whether this reconcile actually invoked
// syncer.UpdateInstance. AppliedAt is the documented "time the controller
// first applied this override" — stamping it on a no-op reconcile
// (cooldown hold) would silently break that contract. Only when didApply
// is true and AppliedAt is still nil do we record it.
//
// Status-update conflicts (HTTP 409) are returned to the caller, which
// surfaces them so controller-runtime can requeue on the latest
// resourceVersion. Conflict on this status write is benign — the next
// reconcile will compute the same desired phase and try again.
func (r *SpannerAutoscalerReconciler) updateManualScalingProgress(
	ctx context.Context,
	ms *spannerv1beta1.SpannerManualScaling,
	currentPU, targetPU int,
	now time.Time,
	didApply bool,
) error {
	var fresh spannerv1beta1.SpannerManualScaling
	if err := r.ctrlClient.Get(ctx, ctrlclient.ObjectKeyFromObject(ms), &fresh); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	desiredPhase := spannerv1beta1.SpannerManualScalingPhaseProgressing
	if currentPU == targetPU {
		desiredPhase = spannerv1beta1.SpannerManualScalingPhaseActive
	}

	changed := false
	if fresh.Status.Phase != desiredPhase {
		fresh.Status.Phase = desiredPhase
		changed = true
	}
	if didApply && fresh.Status.AppliedAt == nil {
		fresh.Status.AppliedAt = &metav1.Time{Time: now}
		changed = true
	}
	if desiredPhase == spannerv1beta1.SpannerManualScalingPhaseActive && fresh.Status.ReachedAt == nil {
		fresh.Status.ReachedAt = &metav1.Time{Time: now}
		changed = true
	}
	if fresh.Status.CurrentProcessingUnits != currentPU {
		fresh.Status.CurrentProcessingUnits = currentPU
		changed = true
	}
	if !changed {
		return nil
	}
	return r.ctrlClient.Status().Update(ctx, &fresh)
}

// transitionManualScalingPhase moves a SpannerManualScaling into a terminal
// phase (Expired, Superseded, Invalid). It stamps FinishedAt the first time
// this happens, sets Message for operator visibility, and triggers history
// GC for the namespace.
func (r *SpannerAutoscalerReconciler) transitionManualScalingPhase(
	ctx context.Context,
	ms *spannerv1beta1.SpannerManualScaling,
	phase spannerv1beta1.SpannerManualScalingPhase,
	message string,
	now time.Time,
) error {
	var fresh spannerv1beta1.SpannerManualScaling
	if err := r.ctrlClient.Get(ctx, ctrlclient.ObjectKeyFromObject(ms), &fresh); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if fresh.Status.Phase.IsTerminal() {
		// Already terminal — do not overwrite FinishedAt or Message; the
		// first transition wins.
		return nil
	}
	fresh.Status.Phase = phase
	fresh.Status.Message = message
	fresh.Status.FinishedAt = &metav1.Time{Time: now}
	if err := r.ctrlClient.Status().Update(ctx, &fresh); err != nil {
		return err
	}
	// Trigger history GC for this namespace. It's a no-op when the limit is
	// 0 (default) or when the count is still under the cap.
	if err := r.enforceHistoryLimit(ctx, ms.Namespace); err != nil {
		// GC errors should not block phase transitions; log via the caller.
		return err
	}
	return nil
}

// markExpiredSiblings sweeps SpannerManualScalings targeting sa whose
// ExpiresAt has elapsed and transitions them to phase=Expired. Idempotent:
// already-terminal resources are skipped.
//
// This is the catch-up path that handles the controller having been down
// across an expiry boundary: selectActiveManualScaling skips those
// candidates, but their phase still needs to be updated so the history GC
// can claim them. Running it on every reconcile of the parent keeps the
// state eventually consistent.
//
// The Invalid phase (orphan targetResource) is owned by the dedicated
// SpannerManualScalingReconciler in spannermanualscaling_controller.go,
// which runs for every SpannerManualScaling regardless of whether its
// target exists.
func (r *SpannerAutoscalerReconciler) markExpiredSiblings(
	ctx context.Context,
	log logr.Logger,
	sa *spannerv1beta1.SpannerAutoscaler,
	now time.Time,
) {
	var list spannerv1beta1.SpannerManualScalingList
	if err := r.ctrlClient.List(ctx, &list, ctrlclient.InNamespace(sa.Namespace)); err != nil {
		log.V(1).Info("manual scaling: orphan sweep list failed", "err", err)
		return
	}
	for i := range list.Items {
		ms := &list.Items[i]
		if ms.Spec.TargetResource != sa.Name {
			continue
		}
		if ms.DeletionTimestamp != nil {
			continue
		}
		if ms.Status.Phase.IsTerminal() {
			continue
		}
		if ms.Spec.ExpiresAt != nil && !now.Before(ms.Spec.ExpiresAt.Time) {
			msg := "expiresAt elapsed"
			if currentPU := ms.Status.CurrentProcessingUnits; currentPU != 0 && currentPU != ms.Spec.ProcessingUnits {
				msg = fmt.Sprintf("expiresAt elapsed; target not reached: current=%d target=%d",
					currentPU, ms.Spec.ProcessingUnits)
			}
			if perr := r.transitionManualScalingPhase(ctx, ms,
				spannerv1beta1.SpannerManualScalingPhaseExpired, msg, now); perr != nil {
				log.Error(perr, "manual scaling: failed to mark Expired", "name", ms.Name)
			}
		}
	}
}

// enforceHistoryLimit deletes the oldest terminal SpannerManualScaling
// resources in the namespace beyond r.manualScalingHistoryPerNamespace,
// keeping the N most recent by status.finishedAt (falling back to
// creationTimestamp when finishedAt is unset). No-op when the limit is 0
// (default) or when the namespace is under the cap.
//
// Active / Progressing / Pending resources are never deleted, by design.
func (r *SpannerAutoscalerReconciler) enforceHistoryLimit(ctx context.Context, namespace string) error {
	limit := r.manualScalingHistoryPerNamespace
	if limit <= 0 {
		return nil
	}
	var list spannerv1beta1.SpannerManualScalingList
	if err := r.ctrlClient.List(ctx, &list, ctrlclient.InNamespace(namespace)); err != nil {
		return err
	}
	finished := make([]spannerv1beta1.SpannerManualScaling, 0, len(list.Items))
	for i := range list.Items {
		ms := &list.Items[i]
		if ms.DeletionTimestamp != nil {
			continue
		}
		if ms.Status.Phase.IsTerminal() {
			finished = append(finished, *ms)
		}
	}
	if len(finished) <= limit {
		return nil
	}
	// Sort newest-first by FinishedAt, falling back to CreationTimestamp
	// when the terminal phase write did not capture FinishedAt (legacy
	// rows or controller-side error in transitionManualScalingPhase).
	finishedAt := func(ms spannerv1beta1.SpannerManualScaling) time.Time {
		if ms.Status.FinishedAt != nil {
			return ms.Status.FinishedAt.Time
		}
		return ms.CreationTimestamp.Time
	}
	sort.Slice(finished, func(i, j int) bool {
		return finishedAt(finished[i]).After(finishedAt(finished[j]))
	})
	toDelete := finished[limit:]
	for i := range toDelete {
		if err := r.ctrlClient.Delete(ctx, &toDelete[i]); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		observability.RecordManualScalingHistoryEvicted(toDelete[i].Namespace)
	}
	return nil
}

// manualScalingActiveRamp returns true when the override will step (rather
// than single-jump) in the direction implied by target vs current. The
// caller uses this for the scale_events_total `driver` label and for
// SpannerAutoscaler.status.activeManualScaling.Ramp so both reflect the
// actual transition, not just the spec shape.
//
//   - target > current → consults ScaleupStepSize (scaledown fields are
//     irrelevant for a scale-up).
//   - target < current → consults ScaledownStepSize.
//   - target == current → no transition; returns false (single-jump-equivalent).
func manualScalingActiveRamp(spec *spannerv1beta1.SpannerManualScalingSpec, currentPU int) bool {
	switch {
	case spec.ProcessingUnits > currentPU:
		return spec.ScaleupStepSize != nil
	case spec.ProcessingUnits < currentPU:
		return spec.ScaledownStepSize != nil
	default:
		return false
	}
}

// clearActiveManualScalingState clears sa.Status.ActiveManualScaling (when
// set) and zeroes the manual_scaling_* gauges. Shared by every reconcile
// branch that ends without an active override in effect (no-active, reject
// policy). The caller is responsible for emitting the branch-specific event
// (ManualScalingDeactivated / ManualScalingRejected) BEFORE invoking this.
func (r *SpannerAutoscalerReconciler) clearActiveManualScalingState(sa *spannerv1beta1.SpannerAutoscaler, statusChanged *bool) {
	if sa.Status.ActiveManualScaling != nil {
		sa.Status.ActiveManualScaling = nil
		*statusChanged = true
	}
	labels := observability.LabelsForAutoscaler(sa)
	observability.RecordManualScalingActive(labels, false, false)
	observability.RecordManualScalingTarget(labels, 0, false)
}

// rejectScaledownPolicy handles the defense-in-depth branch where the
// cluster-wide --reject-manual-scaledown flag forbids reducing PU below the
// current value. It marks the active candidate Invalid, emits a Rejected
// event on the parent, then clears the active-override state.
func (r *SpannerAutoscalerReconciler) rejectScaledownPolicy(
	ctx context.Context,
	log logr.Logger,
	sa *spannerv1beta1.SpannerAutoscaler,
	active *spannerv1beta1.SpannerManualScaling,
	now time.Time,
	statusChanged *bool,
) {
	msg := fmt.Sprintf(
		"rejected: scaledown is disabled cluster-wide (--reject-manual-scaledown=true); "+
			"spec.processingUnits=%d < currentProcessingUnits=%d",
		active.Spec.ProcessingUnits, sa.Status.CurrentProcessingUnits)
	log.Info("manual scaling: rejected", "name", active.Name, "reason", msg)
	if perr := r.transitionManualScalingPhase(ctx, active,
		spannerv1beta1.SpannerManualScalingPhaseInvalid, msg, now,
	); perr != nil {
		log.Error(perr, "manual scaling: failed to record rejection")
	}
	r.recorder.Eventf(sa, corev1.EventTypeWarning, "ManualScalingRejected",
		"manual scaling rejected (source=%s): %s", active.Name, msg)
	r.clearActiveManualScalingState(sa, statusChanged)
}

// activeManualScalingEqual compares two *ActiveManualScaling for status-write
// avoidance; nil-nil is equal, nil-nonnil is not, otherwise field-by-field.
// ExpiresAt is compared by absolute instant so trivial timezone re-encoding
// (k8s normalizes to UTC) does not produce spurious diffs.
func activeManualScalingEqual(a, b *spannerv1beta1.ActiveManualScaling) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if a.Name != b.Name || a.ProcessingUnits != b.ProcessingUnits || a.Ramp != b.Ramp {
		return false
	}
	switch {
	case a.ExpiresAt == nil && b.ExpiresAt == nil:
		return true
	case a.ExpiresAt == nil || b.ExpiresAt == nil:
		return false
	default:
		return a.ExpiresAt.Time.Equal(b.ExpiresAt.Time)
	}
}
