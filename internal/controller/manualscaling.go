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

// dispatchManualScaling is the Reconcile-side wrapper around
// reconcileManualScaling. It persists status changes, maps update conflicts
// to a benign requeue, and translates reconcileManualScaling's
// (handled, requeueAfter, err) tuple into the ctrlreconcile.Result shape the
// caller returns. Extracted from Reconcile to keep its cyclomatic complexity
// within the project's golangci-lint threshold.
//
// droppedActiveSchedules carries entries that pruneActiveSchedules already
// removed from sa.Status.CurrentlyActiveSchedules. When this function
// successfully persists sa.Status, it emits the per-entry
// "removed currently active schedule" log line + RecordScheduleDeactivation
// counter for each one. Without this, schedules orphaned in the same
// reconcile as a manual override would have their drop silently swallowed
// (the trailing emission loop in Reconcile's CPU path is skipped when this
// function returns handled=true).
//
// The `handled` return tells the caller whether to short-circuit (true) or
// fall through to the CPU-based autoscale path (false). When handled=false,
// the caller is responsible for emitting the deactivations itself.
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

	// Also sweep any orphan / expired siblings whose ExpiresAt elapsed but
	// the controller never observed them (e.g. controller was down). This
	// keeps phase=Expired/Invalid eventually consistent without requiring a
	// dedicated SpannerManualScaling reconciler.
	r.markExpiredOrInvalidSiblings(ctx, log, sa, now)

	if active == nil {
		// No active override. If this autoscaler was previously holding a
		// manual override, clear the status and emit Deactivated.
		if sa.Status.ActiveManualScaling != nil {
			r.recorder.Eventf(sa, corev1.EventTypeNormal, "ManualScalingDeactivated",
				"manual scaling deactivated (no active SpannerManualScaling for this target)")
			sa.Status.ActiveManualScaling = nil
			*statusChanged = true
		}
		// Zero the manual_scaling_* gauges so previous readings don't
		// linger.
		observability.RecordManualScalingActive(observability.LabelsForAutoscaler(sa), false, false)
		observability.RecordManualScalingTarget(observability.LabelsForAutoscaler(sa), 0, false)
		return false, 0, nil
	}

	// We have an Active candidate. Defense-in-depth scaledown check.
	if r.rejectManualScaledown && active.Spec.ProcessingUnits < sa.Status.CurrentProcessingUnits {
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
		if sa.Status.ActiveManualScaling != nil {
			sa.Status.ActiveManualScaling = nil
			*statusChanged = true
		}
		// Zero the manual_scaling_* gauges so a previously-active gauge
		// reading does not linger past the rejection (operator switches
		// --reject-manual-scaledown true mid-flight, or webhook is bypassed).
		labels := observability.LabelsForAutoscaler(sa)
		observability.RecordManualScalingActive(labels, false, false)
		observability.RecordManualScalingTarget(labels, 0, false)
		return false, 0, nil
	}

	// We will apply the override. Suppress schedule-derived min/max so
	// DesiredProcessingUnits is bounded by the manual target only.
	if sa.Status.DesiredMinPUs != 0 || sa.Status.DesiredMaxPUs != 0 {
		sa.Status.DesiredMinPUs = 0
		sa.Status.DesiredMaxPUs = 0
		*statusChanged = true
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
	if r.needApplyManualScaling(sa, nextPU) {
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
		if isTerminalManualScalingPhase(ms.Status.Phase) {
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
func (r *SpannerAutoscalerReconciler) nextManualPU(
	sa *spannerv1beta1.SpannerAutoscaler,
	ms *spannerv1beta1.SpannerManualScaling,
	now time.Time,
) (int, time.Duration) {
	target := ms.Spec.ProcessingUnits
	current := sa.Status.CurrentProcessingUnits
	if current == target {
		return target, 0
	}

	var stepSize int
	var interval time.Duration
	if target > current {
		if ms.Spec.ScaleupStepSize == nil {
			return target, 0 // single-jump scale-up
		}
		stepSize = resolveStepSize(ms.Spec.ScaleupStepSize, current, stepDirectionScaleup)
		interval = durationOr(ms.Spec.ScaleupInterval, r.scaleUpInterval)
	} else {
		if ms.Spec.ScaledownStepSize == nil {
			return target, 0 // single-jump scale-down
		}
		stepSize = resolveStepSize(ms.Spec.ScaledownStepSize, current, stepDirectionScaledown)
		interval = durationOr(ms.Spec.ScaledownInterval, r.scaleDownInterval)
	}

	// Honor cooldown: if LastScaleTime + interval has not elapsed, do not
	// change PU this round; schedule a wakeup at the cooldown boundary.
	if !sa.Status.LastScaleTime.IsZero() && interval > 0 {
		elapsed := now.Sub(sa.Status.LastScaleTime.Time)
		if elapsed < interval {
			return current, interval - elapsed
		}
	}

	next := target
	if stepSize > 0 {
		if target > current {
			next = current + stepSize
			if next > target {
				next = target
			}
		} else {
			next = current - stepSize
			if next < target {
				next = target
			}
		}
	}
	return next, interval
}

// needApplyManualScaling returns true when nextPU differs from currentPU and
// thus a syncer.UpdateInstance call is required this reconcile. The
// same-value case is reported as SkipReasonSame so the existing skip metric
// records "nothing to do" reconciles uniformly.
func (r *SpannerAutoscalerReconciler) needApplyManualScaling(sa *spannerv1beta1.SpannerAutoscaler, nextPU int) bool {
	return nextPU != sa.Status.CurrentProcessingUnits
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
	if isTerminalManualScalingPhase(fresh.Status.Phase) {
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

// markExpiredOrInvalidSiblings sweeps SpannerManualScalings targeting sa for
// (a) ExpiresAt in the past and (b) target SpannerAutoscaler does not match
// any existing resource (the spec.targetResource is wrong). The first case
// gets phase=Expired; the second gets phase=Invalid. Both transitions are
// idempotent (already-terminal resources are skipped).
//
// This is the catch-up path: selectActiveManualScaling skips these
// candidates, but their phase needs to be updated to Expired/Invalid so the
// history GC can claim them. Running this in every reconcile of the parent
// keeps things eventually consistent without a dedicated controller for
// SpannerManualScaling.
func (r *SpannerAutoscalerReconciler) markExpiredOrInvalidSiblings(
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
		if isTerminalManualScalingPhase(ms.Status.Phase) {
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
		if isTerminalManualScalingPhase(ms.Status.Phase) {
			finished = append(finished, *ms)
		}
	}
	if len(finished) <= limit {
		return nil
	}
	// Sort newest-first by FinishedAt (fallback: CreationTimestamp).
	sort.Slice(finished, func(i, j int) bool {
		ti := finishedSortKey(finished[i])
		tj := finishedSortKey(finished[j])
		return ti.After(tj)
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

// finishedSortKey returns the timestamp used to order terminal
// SpannerManualScaling resources newest-first for the history-limit GC.
func finishedSortKey(ms spannerv1beta1.SpannerManualScaling) time.Time {
	if ms.Status.FinishedAt != nil {
		return ms.Status.FinishedAt.Time
	}
	return ms.CreationTimestamp.Time
}

// isTerminalManualScalingPhase reports whether p is a phase the history GC
// and the active-candidate selector both treat as done.
func isTerminalManualScalingPhase(p spannerv1beta1.SpannerManualScalingPhase) bool {
	switch p {
	case spannerv1beta1.SpannerManualScalingPhaseExpired,
		spannerv1beta1.SpannerManualScalingPhaseSuperseded,
		spannerv1beta1.SpannerManualScalingPhaseInvalid:
		return true
	}
	return false
}

// manualScalingHasRamp returns true when the spec configures stepped scaling
// in either direction (presence of scaleupStepSize or scaledownStepSize).
// Interval-only specification does not count as ramp — the webhook surfaces
// that combination as a warning.
//
// Most call sites should prefer manualScalingActiveRamp because it reflects
// what THIS reconcile will actually do (stepped vs single-jump for the
// direction being driven), whereas this function answers the broader
// "anywhere in the spec" question.
func manualScalingHasRamp(spec *spannerv1beta1.SpannerManualScalingSpec) bool {
	return spec.ScaleupStepSize != nil || spec.ScaledownStepSize != nil
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
//
// This deliberately diverges from manualScalingHasRamp, which OR's both
// directions for the spec-level "is any ramp configured" question.
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

// durationOr returns d.Duration when d is non-nil, otherwise fallback.
func durationOr(d *metav1.Duration, fallback time.Duration) time.Duration {
	if d == nil {
		return fallback
	}
	return d.Duration
}
