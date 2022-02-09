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

package controllers

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilclock "k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/client-go/tools/record"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	ctrlmanager "sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	"github.com/mercari/spanner-autoscaler/internal/metrics"
	"github.com/mercari/spanner-autoscaler/internal/spanner"
	"github.com/mercari/spanner-autoscaler/internal/syncer"
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

	syncers map[types.NamespacedName]syncer.Syncer

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
func WithSyncers(syncers map[types.NamespacedName]syncer.Syncer) Option {
	return withSyncers{syncers: syncers}
}

type withSyncers struct {
	syncers map[types.NamespacedName]syncer.Syncer
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
		syncers:           make(map[types.NamespacedName]syncer.Syncer),
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
	nn := req.NamespacedName
	log := r.log.WithValues("namespaced name", nn)

	r.mu.RLock()
	s, syncerExists := r.syncers[nn]
	r.mu.RUnlock()

	var sa spannerv1beta1.SpannerAutoscaler
	if err := r.ctrlClient.Get(ctx, nn, &sa); err != nil {
		err = ctrlclient.IgnoreNotFound(err)
		if err != nil {
			log.Error(err, "failed to get spanner-autoscaler")
			return ctrlreconcile.Result{}, err
		}

		log.V(2).Info("checking if a syncer exists")
		if syncerExists {
			s.Stop()
			r.mu.Lock()
			delete(r.syncers, nn)
			r.mu.Unlock()
			log.Info("stopped syncer")
		}

		return ctrlreconcile.Result{}, nil
	}

	log.V(1).Info("resource status", "spannerautoscaler", sa)

	credentials, err := r.fetchCredentials(ctx, &sa)
	if err != nil {
		r.recorder.Event(&sa, corev1.EventTypeWarning, "ServiceAccountRequired", err.Error())
		log.Error(err, "failed to fetch service account")
		return ctrlreconcile.Result{}, err
	}

	// If the syncer does not exist, start a syncer.
	if !syncerExists {
		log.V(2).Info("syncer does not exist, starting a new syncer")

		if err := r.startSyncer(ctx, log, nn, sa.Spec.TargetInstance.ProjectID, sa.Spec.TargetInstance.InstanceID, credentials); err != nil {
			r.recorder.Event(&sa, corev1.EventTypeWarning, "FailedStartSyncer", err.Error())
			log.Error(err, "failed to start syncer")
			return ctrlreconcile.Result{}, err
		}

		return ctrlreconcile.Result{}, nil
	}

	// If credentials have been changed, then just replace syncer.
	if !s.HasCredentials(credentials) {
		s.Stop()
		r.mu.Lock()
		delete(r.syncers, nn)
		r.mu.Unlock()

		if err := r.startSyncer(ctx, log, nn, sa.Spec.TargetInstance.ProjectID, sa.Spec.TargetInstance.InstanceID, credentials); err != nil {
			r.recorder.Event(&sa, corev1.EventTypeWarning, "FailedStartSyncer", err.Error())
			log.Error(err, "failed to start syncer")
			return ctrlreconcile.Result{}, err
		}

		log.Info("replaced syncer", "namespaced name", sa)
		return ctrlreconcile.Result{}, nil
	}

	if sa.Status.InstanceState != spanner.StateReady {
		log.Info("instance state is not ready")
		return ctrlreconcile.Result{}, nil
	}

	if sa.Status.CurrentProcessingUnits == 0 {
		log.Info("current processing units have not fetched yet")
		return ctrlreconcile.Result{}, nil
	}

	desiredProcessingUnits := calcDesiredProcessingUnits(sa)

	now := r.clock.Now()

	if !r.needUpdateProcessingUnits(log, &sa, desiredProcessingUnits, now) {
		return ctrlreconcile.Result{}, nil
	}

	log.V(1).Info("processing units need to be changed", "desiredProcessingUnits", desiredProcessingUnits, "sa.Status", sa.Status)
	if err := s.UpdateInstance(ctx, desiredProcessingUnits); err != nil {
		r.recorder.Event(&sa, corev1.EventTypeWarning, "FailedUpdateInstance", err.Error())
		log.Error(err, "failed to update spanner instance status")
		return ctrlreconcile.Result{}, err
	}

	r.recorder.Eventf(&sa, corev1.EventTypeNormal, "Updated", "Updated processing units of %s/%s from %d to %d", sa.Spec.TargetInstance.ProjectID, sa.Spec.TargetInstance.InstanceID,
		sa.Status.CurrentProcessingUnits, desiredProcessingUnits)

	log.Info("updated processing units via google cloud api", "before", sa.Status.CurrentProcessingUnits, "after", desiredProcessingUnits)

	saCopy := sa.DeepCopy()
	saCopy.Status.DesiredProcessingUnits = desiredProcessingUnits
	saCopy.Status.DesiredNodes = desiredProcessingUnits / 1000
	saCopy.Status.LastScaleTime = metav1.Time{Time: now}

	if err = r.ctrlClient.Status().Update(ctx, saCopy); err != nil {
		r.recorder.Event(&sa, corev1.EventTypeWarning, "FailedUpdateStatus", err.Error())
		log.Error(err, "failed to update spanner autoscaler status")
		return ctrlreconcile.Result{}, err
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

func (r *SpannerAutoscalerReconciler) startSyncer(ctx context.Context, log logr.Logger, nn types.NamespacedName, projectID, instanceID string, credentials *syncer.Credentials) error {
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

	s, err := syncer.New(ctx, r.ctrlClient, nn, credentials, r.recorder, spannerClient, metricsClient, syncer.WithLog(log))
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
		log.Info("no need to scale", "current-processing-units", currentProcessingUnits, "current-cpu", sa.Status.CurrentHighPriorityCPUUtilization)
		return false

	case desiredProcessingUnits > currentProcessingUnits && r.clock.Now().Before(sa.Status.LastScaleTime.Time.Add(10*time.Second)):
		log.Info("too short to scale up since last scale-up event",
			"time gap", now.Sub(sa.Status.LastScaleTime.Time),
			"now", now.String(),
			"last scale time", sa.Status.LastScaleTime,
		)

		return false

	case desiredProcessingUnits < currentProcessingUnits && r.clock.Now().Before(sa.Status.LastScaleTime.Time.Add(r.scaleDownInterval)):
		log.Info("too short to scale down since last scale-up event",
			"time gap", now.Sub(sa.Status.LastScaleTime.Time),
			"now", now.String(),
			"last scale time", sa.Status.LastScaleTime,
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

	// in case of scaling down, check that we don't scale down beyond the ScaledownStepSize
	if scaledDownPU := (sa.Status.CurrentProcessingUnits - sa.Spec.ScaleConfig.ScaledownStepSize); desiredPU < scaledDownPU {
		desiredPU = scaledDownPU
	}

	// keep the scaling between the specified min/max range
	if desiredPU < sa.Spec.ScaleConfig.ProcessingUnits.Min {
		desiredPU = sa.Spec.ScaleConfig.ProcessingUnits.Min
	}

	if desiredPU > sa.Spec.ScaleConfig.ProcessingUnits.Max {
		desiredPU = sa.Spec.ScaleConfig.ProcessingUnits.Max
	}

	return desiredPU
}

func (r *SpannerAutoscalerReconciler) fetchCredentials(ctx context.Context, sa *spannerv1beta1.SpannerAutoscaler) (*syncer.Credentials, error) {
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

		return syncer.NewServiceAccountJSONCredentials(serviceAccountJSON), nil

	case spannerv1beta1.AuthTypeImpersonation:
		return syncer.NewServiceAccountImpersonate(impersonateConfig.TargetServiceAccount, impersonateConfig.Delegates), nil

	default:
		return syncer.NewADCCredentials(), nil
	}
}
