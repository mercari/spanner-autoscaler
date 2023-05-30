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
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	cronpkg "github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	utilclock "k8s.io/utils/clock"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	ctrlmanager "sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	"github.com/mercari/spanner-autoscaler/internal/metrics"
	schedulerpkg "github.com/mercari/spanner-autoscaler/internal/scheduler"
	"github.com/mercari/spanner-autoscaler/internal/spanner"
	syncerpkg "github.com/mercari/spanner-autoscaler/internal/syncer"
)

var (
	errFetchServiceAccountJSONNoNameSpecified   = errors.New("no name specified")
	errFetchServiceAccountJSONNoKeySpecified    = errors.New("no key specified")
	errFetchServiceAccountJSONNoSecretFound     = errors.New("no secret found by specified name")
	errFetchServiceAccountJSONNoSecretDataFound = errors.New("no secret found by specified key")
	errInvalidExclusiveCredentials              = errors.New("impersonateConfig and iamKeySecret are mutually exclusive")
)

// SpannerAutoscalerReconciler reconciles a SpannerAutoscaler object.
type SpannerAutoscalerReconciler struct {
	ctrlClient ctrlclient.Client
	apiReader  ctrlclient.Reader

	scheme   *runtime.Scheme
	recorder record.EventRecorder

	syncers    map[types.NamespacedName]syncerpkg.Syncer
	crons      map[types.NamespacedName]*cronpkg.Cron
	schedulers map[types.NamespacedName]schedulerpkg.Scheduler

	scaleDownInterval time.Duration

	clock utilclock.Clock
	log   logr.Logger
	mu    sync.RWMutex
}

var _ ctrlreconcile.Reconciler = (*SpannerAutoscalerReconciler)(nil)

// Functional options used in this package, follow the last pattern shown here:
// https://sagikazarmark.hu/blog/functional-options-on-steroids/

// Option provides an extensible set of functional options for configuring autoscaler-reconciler
// as well as schedule-reconciler
type Option interface {
	// Uncomment the following 2 lines only if the schedule-reconciler and the autoscaler-reconciler
	// consume same options. But since schedule-reconciler implements only a subset of the autoscaler-reconciler
	// options (only WithLog), it can not be embedded in this interface.

	// This can be uncommented (because schedule-reconciler is a subset of this), but has been
	// kept so, just for reducing complexity and for increasing readability
	// SpannerAutoscalerReconcilerOption

	// SpannerAutoscaleScheduleReconcilerOption
}

// SpannerAutoscalerReconcilerOption is a subset the Option interface, for autoscaler-reconciler
type SpannerAutoscalerReconcilerOption interface {
	applySpannerAutoscalerReconciler(r *SpannerAutoscalerReconciler)
}

// Add logger option for the autoscaler-reconciler
func WithLog(log logr.Logger) Option {
	return withLog{logger: log}
}

type withLog struct {
	logger logr.Logger
}

func (o withLog) applySpannerAutoscalerReconciler(r *SpannerAutoscalerReconciler) {
	r.log = o.logger.WithName("spannerautoscaler")
}

// Add syncer option for the autoscaler-reconciler
func WithSyncers(syncers map[types.NamespacedName]syncerpkg.Syncer) Option {
	return withSyncers{syncers: syncers}
}

type withSyncers struct {
	syncers map[types.NamespacedName]syncerpkg.Syncer
}

func (o withSyncers) applySpannerAutoscalerReconciler(r *SpannerAutoscalerReconciler) {
	r.syncers = o.syncers
}

// Add clock option for the autoscaler-reconciler
func WithClock(clock utilclock.Clock) Option {
	return withClock{clock: clock}
}

type withClock struct {
	clock utilclock.Clock
}

func (o withClock) applySpannerAutoscalerReconciler(r *SpannerAutoscalerReconciler) {
	r.clock = o.clock
}

// Add scale-down-interval option for the autoscaler-reconciler
func WithScaleDownInterval(scaleDownInterval time.Duration) Option {
	return withScaleDownInterval{scaleDownInterval: scaleDownInterval}
}

type withScaleDownInterval struct {
	scaleDownInterval time.Duration
}

func (o withScaleDownInterval) applySpannerAutoscalerReconciler(r *SpannerAutoscalerReconciler) {
	r.scaleDownInterval = o.scaleDownInterval
}

// NewSpannerAutoscalerReconciler returns a new SpannerAutoscalerReconciler.
func NewSpannerAutoscalerReconciler(
	ctrlClient ctrlclient.Client,
	apiReader ctrlclient.Reader,
	scheme *runtime.Scheme,
	recorder record.EventRecorder,
	logger logr.Logger,
	opts ...Option,
) *SpannerAutoscalerReconciler {
	r := &SpannerAutoscalerReconciler{
		ctrlClient:        ctrlClient,
		apiReader:         apiReader,
		scheme:            scheme,
		recorder:          recorder,
		syncers:           make(map[types.NamespacedName]syncerpkg.Syncer),
		schedulers:        make(map[types.NamespacedName]schedulerpkg.Scheduler),
		crons:             make(map[types.NamespacedName]*cronpkg.Cron),
		scaleDownInterval: 55 * time.Minute,
		clock:             utilclock.RealClock{},
		log:               logger,
	}

	for _, option := range opts {
		if opt, ok := option.(SpannerAutoscalerReconcilerOption); ok {
			opt.applySpannerAutoscalerReconciler(r)
		}
	}
	return r
}

// +kubebuilder:rbac:groups=spanner.mercari.com,resources=spannerautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=spanner.mercari.com,resources=spannerautoscalers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=spanner.mercari.com,resources=spannerautoscalers/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get,resourceNames=spanner-autoscaler-gcp-sa

// Reconcile implements ctrlreconcile.Reconciler.
func (r *SpannerAutoscalerReconciler) Reconcile(ctx context.Context, req ctrlreconcile.Request) (ctrlreconcile.Result, error) {
	nnsa := req.NamespacedName
	log := r.log.WithValues("autoscaler", nnsa)
	statusChanged := false

	r.mu.RLock()
	syncer, syncerExists := r.syncers[nnsa]
	cron, cronExists := r.crons[nnsa]
	scheduler, schedulerExists := r.schedulers[nnsa]
	r.mu.RUnlock()

	var sa spannerv1beta1.SpannerAutoscaler
	if err := r.ctrlClient.Get(ctx, nnsa, &sa); err != nil {
		if apierrors.IsNotFound(err) {
			// Remove cron and syncer if the resource does not exist
			if syncerExists {
				log.V(2).Info("removing syncer for non-existent resource")
				syncer.Stop()
				r.mu.Lock()
				delete(r.syncers, nnsa)
				r.mu.Unlock()
				log.Info("stopped syncer")
			}

			if cronExists {
				cron.Stop()
				r.mu.Lock()
				delete(r.crons, nnsa)
				r.mu.Unlock()
				log.Info("removed cron")
			}

			if schedulerExists {
				scheduler.Stop()
				r.mu.Lock()
				delete(r.schedulers, nnsa)
				r.mu.Unlock()
				log.Info("removed scheduler")
			}

			return ctrlreconcile.Result{}, nil
		} else {
			log.Error(err, "failed to get spanner-autoscaler")
			return ctrlreconcile.Result{}, err
		}
	}

	credentials, err := r.fetchCredentials(ctx, &sa)
	if err != nil {
		r.recorder.Event(&sa, corev1.EventTypeWarning, "ServiceAccountRequired", err.Error())
		log.Error(err, "failed to fetch service account")
		return ctrlreconcile.Result{}, err
	}

	// If the syncer does not exist, start a syncer.
	if !syncerExists {
		log.V(2).Info("syncer does not exist, starting a new syncer")

		if err := r.startSyncer(ctx, log, nnsa, sa.Spec.TargetInstance.ProjectID, sa.Spec.TargetInstance.InstanceID, credentials); err != nil {
			r.recorder.Event(&sa, corev1.EventTypeWarning, "FailedStartSyncer", err.Error())
			log.Error(err, "failed to start syncer")
			return ctrlreconcile.Result{}, err
		}

		return ctrlreconcile.Result{}, nil
	}

	// If credentials have been changed, then just replace syncer.
	if !syncer.HasCredentials(credentials) {
		syncer.Stop()
		r.mu.Lock()
		delete(r.syncers, nnsa)
		r.mu.Unlock()

		if err := r.startSyncer(ctx, log, nnsa, sa.Spec.TargetInstance.ProjectID, sa.Spec.TargetInstance.InstanceID, credentials); err != nil {
			r.recorder.Event(&sa, corev1.EventTypeWarning, "FailedStartSyncer", err.Error())
			log.Error(err, "failed to start syncer")
			return ctrlreconcile.Result{}, err
		}

		log.Info("replaced syncer", "namespacedName", sa)
		return ctrlreconcile.Result{}, nil
	}

	if !cronExists && len(sa.Status.Schedules) > 0 {
		cron = cronpkg.New(cronpkg.WithChain(
			cronpkg.Recover(log),
		))
		cron.Start()

		scheduler = schedulerpkg.New(log, r.ctrlClient, r.crons, nnsa)
		go scheduler.Start()

		r.mu.Lock()
		r.crons[nnsa] = cron
		cronExists = true
		r.schedulers[nnsa] = scheduler
		r.mu.Unlock()
	}

	if err := r.addCronJob(ctx, log, sa, cron); err != nil {
		return ctrlreconcile.Result{}, err
	}

	// TODO: implement cronjob schedule update whenever SpannerAutoscaleSchedule cron is changed (or block changes to Schedule after creation)

	if cronExists {
		pruneCronJobs(log, sa, cron)
	}

	if newActiveSchedules, updated := pruneActiveSchedules(log, sa); updated {
		sa.Status.CurrentlyActiveSchedules = newActiveSchedules
		statusChanged = true
	}

	if sa.Status.InstanceState != spanner.StateReady {
		log.Info("instance state is not ready")
		return ctrlreconcile.Result{}, nil
	}

	if sa.Status.CurrentProcessingUnits == 0 {
		log.Info("current processing units have not fetched yet")
		return ctrlreconcile.Result{}, nil
	}

	if minPU, maxPU, updated := calcDesiredPURange(sa); updated {
		sa.Status.DesiredMinPUs = minPU
		sa.Status.DesiredMaxPUs = maxPU
		statusChanged = true
	}

	desiredProcessingUnits := calcDesiredProcessingUnits(sa)

	if now := r.clock.Now(); r.needUpdateProcessingUnits(log, &sa, desiredProcessingUnits, now) {
		log.V(1).Info("processing units need to be changed", "desiredProcessingUnits", desiredProcessingUnits, "sa.Status", sa.Status)

		if err := syncer.UpdateInstance(ctx, desiredProcessingUnits); err != nil {
			r.recorder.Event(&sa, corev1.EventTypeWarning, "FailedUpdateInstance", err.Error())
			log.Error(err, "failed to update spanner instance")
			return ctrlreconcile.Result{}, err
		}

		r.recorder.Eventf(&sa, corev1.EventTypeNormal, "Updated", "Updated processing units of %s/%s from %d to %d", sa.Spec.TargetInstance.ProjectID, sa.Spec.TargetInstance.InstanceID,
			sa.Status.CurrentProcessingUnits, desiredProcessingUnits)

		log.Info("updated processing units via google cloud api", "before", sa.Status.CurrentProcessingUnits, "after", desiredProcessingUnits)

		statusChanged = true
		sa.Status.LastScaleTime = metav1.Time{Time: now}
	}

	if statusChanged {
		sa.Status.DesiredProcessingUnits = desiredProcessingUnits

		if err = r.ctrlClient.Status().Update(ctx, &sa); err != nil {
			r.recorder.Event(&sa, corev1.EventTypeWarning, "FailedUpdateStatus", err.Error())
			log.Error(err, "failed to update spanner autoscaler status")
			return ctrlreconcile.Result{}, err
		}
	}

	return ctrlreconcile.Result{}, nil
}

// SetupWithManager sets up the controller with ctrlmanager.Manager.
func (r *SpannerAutoscalerReconciler) SetupWithManager(mgr ctrlmanager.Manager) error {
	opts := ctrlcontroller.Options{
		Reconciler: r,
	}

	return ctrlbuilder.ControllerManagedBy(mgr).
		For(&spannerv1beta1.SpannerAutoscaler{}).
		WithOptions(opts).
		Complete(r)
}

func (r *SpannerAutoscalerReconciler) addCronJob(ctx context.Context, log logr.Logger, sa spannerv1beta1.SpannerAutoscaler, cron *cronpkg.Cron) error {
	for _, v := range sa.Status.Schedules {
		var sas spannerv1beta1.SpannerAutoscaleSchedule
		nnsa := ctrlclient.ObjectKeyFromObject(&sa)
		nnsas := parseNamespacedName(v, sa.ObjectMeta.Namespace)
		if err := r.ctrlClient.Get(ctx, nnsas, &sas); err != nil {
			log.Error(err, "failed to get spanner-autoscaler-schedule")
			return err
		} else {
			job := schedulerpkg.Job{
				ScheduleName:   nnsas,
				AutoscalerName: nnsa,
				Log:            log.WithName("job").WithValues("schedule", nnsas, "autoscaler", nnsa),
				CtrlClient:     r.ctrlClient,
			}
			if !jobExists(cron, job) {
				if _, err := cron.AddJob(sas.Spec.Schedule.Cron, job); err != nil {
					log.Error(err, "failed to add cron job to the cron")
				}
				log.Info("new cron job has been registered", "cron", sas.Spec.Schedule.Cron, "schedule", nnsas.String())
			}
		}
	}
	return nil
}

func pruneCronJobs(log logr.Logger, sa spannerv1beta1.SpannerAutoscaler, cron *cronpkg.Cron) {
	for _, entry := range cron.Entries() {
		job := entry.Job.(schedulerpkg.Job)
		if _, found := findInArray(sa.Status.Schedules, job.ScheduleName.String()); !found {
			cron.Remove(entry.ID)
			log.V(1).Info("removed cron job", "job", job, "cronjob-count", len(cron.Entries()))
		}
	}
}

func pruneActiveSchedules(log logr.Logger, sa spannerv1beta1.SpannerAutoscaler) ([]spannerv1beta1.ActiveSchedule, bool) {
	result := []spannerv1beta1.ActiveSchedule{}
	changed := false
	for _, as := range sa.Status.CurrentlyActiveSchedules {
		if _, found := findInArray(sa.Status.Schedules, as.ScheduleName); !found {
			log.V(1).Info("removed currently active schedule", "ActiveSchedule", as)
			changed = true
			continue
		}
		result = append(result, as)
	}
	return result, changed
}

func jobExists(cron *cronpkg.Cron, job schedulerpkg.Job) bool {
	for _, e := range cron.Entries() {
		entryJob := e.Job.(schedulerpkg.Job)
		if entryJob.ScheduleName == job.ScheduleName {
			return true
		}
	}
	return false
}

// TODO: move this (and other similar functions) to 'internal/util' package
func parseNamespacedName(name string, defaultNamespace string) types.NamespacedName {
	result := types.NamespacedName{}
	arr := strings.SplitN(name, "/", 2)
	if len(arr) == 2 {
		result.Name = arr[1]
		result.Namespace = arr[0]
	} else {
		result.Name = arr[0]
		result.Namespace = defaultNamespace
	}
	return result
}

func (r *SpannerAutoscalerReconciler) startSyncer(ctx context.Context, log logr.Logger, nn types.NamespacedName, projectID, instanceID string, credentials *syncerpkg.Credentials) error {
	ts, err := credentials.TokenSource(ctx)
	if err != nil {
		return err
	}

	spannerClient, err := spanner.NewClient(
		ctx,
		projectID,
		instanceID,
		spanner.WithTokenSource(ts),
		spanner.WithLog(log),
	)
	if err != nil {
		return err
	}

	metricsClient, err := metrics.NewClient(
		ctx,
		projectID,
		instanceID,
		metrics.WithTokenSource(ts),
		metrics.WithLog(log),
	)
	if err != nil {
		return err
	}

	s, err := syncerpkg.New(ctx, r.ctrlClient, nn, credentials, r.recorder, spannerClient, metricsClient, syncerpkg.WithLog(log))
	if err != nil {
		return err
	}

	go s.Start()
	r.mu.Lock()
	r.syncers[nn] = s
	r.mu.Unlock()

	log.V(1).Info("added syncer")

	return nil
}

func (r *SpannerAutoscalerReconciler) needUpdateProcessingUnits(log logr.Logger, sa *spannerv1beta1.SpannerAutoscaler, desiredProcessingUnits int, now time.Time) bool {
	currentProcessingUnits := sa.Status.CurrentProcessingUnits

	switch {
	case desiredProcessingUnits == currentProcessingUnits:
		log.Info("no need to scale", "currentPU", currentProcessingUnits, "currentCPU", sa.Status.CurrentHighPriorityCPUUtilization)
		return false

	case desiredProcessingUnits > currentProcessingUnits && r.clock.Now().Before(sa.Status.LastScaleTime.Time.Add(10*time.Second)):
		log.Info("too short to scale up since last scale-up event",
			"timeGap", now.Sub(sa.Status.LastScaleTime.Time).String(),
			"now", now.String(),
			"lastScaleTime", sa.Status.LastScaleTime,
			"currentPU", currentProcessingUnits,
			"desiredPU", desiredProcessingUnits,
		)
		return false

	case desiredProcessingUnits < currentProcessingUnits && r.clock.Now().Before(sa.Status.LastScaleTime.Time.Add(getOrConvertTimeDuration(sa.Spec.ScaleConfig.ScaledownInterval, r.scaleDownInterval))):
		log.Info("too short to scale down since last scale-up event",
			"timeGap", now.Sub(sa.Status.LastScaleTime.Time).String(),
			"now", now.String(),
			"lastScaleTime", sa.Status.LastScaleTime,
			"currentPU", currentProcessingUnits,
			"desiredPU", desiredProcessingUnits,
		)
		return false

	default:
		return true
	}
}

// calcDesiredProcessingUnits calculates the values needed to keep CPU utilization below TargetCPU.
func calcDesiredProcessingUnits(sa spannerv1beta1.SpannerAutoscaler) int {
	totalCPU := sa.Status.CurrentHighPriorityCPUUtilization * sa.Status.CurrentProcessingUnits

	requiredPU := totalCPU / sa.Spec.ScaleConfig.TargetCPUUtilization.HighPriority

	var desiredPU int

	// https://cloud.google.com/spanner/docs/compute-capacity?hl=en
	// Valid values for processing units are:
	// If processingUnits < 1000, processing units must be multiples of 100.
	// If processingUnits >= 1000, processing units must be multiples of 1000.
	//
	// Round up the requiredPU value to make it valid
	// If it is already a valid PU, increment to next unit to keep CPU usage below desired threshold
	if requiredPU < 1000 {
		desiredPU = ((requiredPU / 100) + 1) * 100
	} else {
		desiredPU = ((requiredPU / 1000) + 1) * 1000
	}

	sdStepSize := sa.Spec.ScaleConfig.ScaledownStepSize

	// round up the scaledownStepSize to avoid intermediate values
	// for example: 8000 -> 7000 instead of 8000 -> 7400
	if sdStepSize < 1000 && sa.Status.CurrentProcessingUnits > 1000 {
		sdStepSize = 1000
	}

	// in case of scaling down, check that we don't scale down beyond the ScaledownStepSize
	if scaledDownPU := (sa.Status.CurrentProcessingUnits - sdStepSize); desiredPU < scaledDownPU {
		desiredPU = scaledDownPU
	}

	// keep the scaling between the specified min/max range
	minPU := sa.Spec.ScaleConfig.ProcessingUnits.Min
	maxPU := sa.Spec.ScaleConfig.ProcessingUnits.Max

	// fetch min/max range from status, in case any schedules have updated the range
	if sa.Status.DesiredMinPUs > 0 {
		minPU = sa.Status.DesiredMinPUs
	}
	if sa.Status.DesiredMaxPUs > 0 {
		maxPU = sa.Status.DesiredMaxPUs
	}

	if desiredPU < minPU {
		desiredPU = minPU
	}

	if desiredPU > maxPU {
		desiredPU = maxPU
	}

	return desiredPU
}

func calcDesiredPURange(sa spannerv1beta1.SpannerAutoscaler) (int, int, bool) {
	changed := false
	var desiredMin, desiredMax int
	for i, sched := range sa.Status.CurrentlyActiveSchedules {
		if i == 0 {
			desiredMin = sched.AdditionalPU
		} else if sched.AdditionalPU < desiredMin {
			desiredMin = sched.AdditionalPU
		}
		desiredMax += sched.AdditionalPU
	}

	desiredMin += sa.Spec.ScaleConfig.ProcessingUnits.Min
	desiredMax += sa.Spec.ScaleConfig.ProcessingUnits.Max

	// round up, in case any schedule adds small number of PUs
	if remainder := desiredMin % 1000; desiredMin > 1000 && remainder != 0 {
		desiredMin = ((desiredMin / 1000) + 1) * 1000
	}

	if remainder := desiredMax % 1000; desiredMax > 1000 && remainder != 0 {
		desiredMax = ((desiredMax / 1000) + 1) * 1000
	}

	if desiredMin != sa.Status.DesiredMinPUs || desiredMax != sa.Status.DesiredMaxPUs {
		changed = true
	}

	return desiredMin, desiredMax, changed
}

func (r *SpannerAutoscalerReconciler) fetchCredentials(ctx context.Context, sa *spannerv1beta1.SpannerAutoscaler) (*syncerpkg.Credentials, error) {
	iamKeySecret := sa.Spec.Authentication.IAMKeySecret
	impersonateConfig := sa.Spec.Authentication.ImpersonateConfig

	// TODO: move this to 'validating' webhook
	if iamKeySecret != nil && impersonateConfig != nil {
		return nil, errInvalidExclusiveCredentials
	}

	switch sa.Spec.Authentication.Type {
	case spannerv1beta1.AuthTypeSA:
		if iamKeySecret.Name == "" {
			return nil, errFetchServiceAccountJSONNoNameSpecified
		}

		if iamKeySecret.Key == "" {
			return nil, errFetchServiceAccountJSONNoKeySpecified
		}

		var namespace string
		// TODO: move this to 'defaulting' webhook
		if iamKeySecret.Namespace == "" {
			namespace = sa.Namespace
		} else {
			namespace = iamKeySecret.Namespace
		}

		var secret corev1.Secret
		key := ctrlclient.ObjectKey{
			Name:      iamKeySecret.Name,
			Namespace: namespace,
		}
		if err := r.apiReader.Get(ctx, key, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, errFetchServiceAccountJSONNoSecretFound
			}
			return nil, err
		}

		serviceAccountJSON, ok := secret.Data[iamKeySecret.Key]
		if !ok {
			return nil, errFetchServiceAccountJSONNoSecretDataFound
		}

		return syncerpkg.NewServiceAccountJSONCredentials(serviceAccountJSON), nil

	case spannerv1beta1.AuthTypeImpersonation:
		return syncerpkg.NewServiceAccountImpersonate(impersonateConfig.TargetServiceAccount, impersonateConfig.Delegates), nil

	default:
		return syncerpkg.NewADCCredentials(), nil
	}
}

func getOrConvertTimeDuration(customDuration *metav1.Duration, defaultDuration time.Duration) time.Duration {
	if customDuration != nil {
		return customDuration.Duration
	}

	return defaultDuration
}
