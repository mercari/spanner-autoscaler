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
	"github.com/mercari/spanner-autoscaler/pkg/logging"
	"github.com/mercari/spanner-autoscaler/pkg/spanner"
	"github.com/mercari/spanner-autoscaler/pkg/syncer"
)

var (
	errFetchServiceAccountJSONNoNameSpecified   = errors.New("no name specified")
	errFetchServiceAccountJSONNoKeySpecified    = errors.New("no key specified")
	errFetchServiceAccountJSONNoSecretFound     = errors.New("no secret found by specified name")
	errFetchServiceAccountJSONNoSecretDataFound = errors.New("no secret found by specified key")
	errInvalidExclusiveCredentials              = errors.New("impersonateConfig and iamKeySecret are mutually exclusive")
)

// TODO: move this to 'defaulting' webhook
const defaultScaledownStepSize = 2

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

type Option func(*SpannerAutoscalerReconciler)

func WithSyncers(syncers map[types.NamespacedName]syncer.Syncer) Option {
	return func(r *SpannerAutoscalerReconciler) {
		r.syncers = syncers
	}
}

func WithScaleDownInterval(scaleDownInterval time.Duration) Option {
	return func(r *SpannerAutoscalerReconciler) {
		r.scaleDownInterval = scaleDownInterval
	}
}

func WithClock(clock utilclock.Clock) Option {
	return func(r *SpannerAutoscalerReconciler) {
		r.clock = clock
	}
}

func WithLog(log logr.Logger) Option {
	return func(r *SpannerAutoscalerReconciler) {
		r.log = log.WithName("spannerautoscaler")
	}
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

	for _, opt := range opts {
		opt(r)
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

	// TODO: move this to the defaulting webhook
	if sa.Spec.ScaleConfig.ScaledownStepSize == 0 {
		sa.Spec.ScaleConfig.ScaledownStepSize = defaultScaledownStepSize
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
		ctx = logging.WithContext(ctx, log)

		if err := r.startSyncer(ctx, nn, sa.Spec.TargetInstance.ProjectID, sa.Spec.TargetInstance.InstanceID, credentials); err != nil {
			r.recorder.Event(&sa, corev1.EventTypeWarning, "FailedStartSyncer", err.Error())
			log.Error(err, "failed to start syncer")
			return ctrlreconcile.Result{}, err
		}

		return ctrlreconcile.Result{}, nil
	}

	// If target spanner instance or service account have been changed, then just replace syncer.
	if s.UpdateTarget(sa.Spec.TargetInstance.ProjectID, sa.Spec.TargetInstance.InstanceID, credentials) {
		s.Stop()
		r.mu.Lock()
		delete(r.syncers, nn)
		r.mu.Unlock()

		if err := r.startSyncer(ctx, nn, sa.Spec.TargetInstance.ProjectID, sa.Spec.TargetInstance.InstanceID, credentials); err != nil {
			r.recorder.Event(&sa, corev1.EventTypeWarning, "FailedStartSyncer", err.Error())
			log.Error(err, "failed to start syncer")
			return ctrlreconcile.Result{}, err
		}

		log.Info("replaced syncer", "namespaced name", sa)
		return ctrlreconcile.Result{}, nil
	}

	log.V(1).Info("checking to see if we need to calculate processing units", "sa", sa)
	if !r.needCalcProcessingUnits(&sa) {
		return ctrlreconcile.Result{}, nil
	}

	// TODO: change this to pass the object instead of so many parameters
	desiredProcessingUnits := calcDesiredProcessingUnits(
		sa.Status.CurrentHighPriorityCPUUtilization,
		normalizeProcessingUnitsOrNodes(sa.Status.CurrentProcessingUnits, sa.Status.CurrentNodes, sa.Spec.ScaleConfig.ComputeType),
		sa.Spec.ScaleConfig.TargetCPUUtilization.HighPriority,
		normalizeProcessingUnitsOrNodes(sa.Spec.ScaleConfig.ProcessingUnits.Min, sa.Spec.ScaleConfig.Nodes.Min, sa.Spec.ScaleConfig.ComputeType),
		normalizeProcessingUnitsOrNodes(sa.Spec.ScaleConfig.ProcessingUnits.Max, sa.Spec.ScaleConfig.Nodes.Max, sa.Spec.ScaleConfig.ComputeType),
		sa.Spec.ScaleConfig.ScaledownStepSize,
	)

	now := r.clock.Now()
	log.V(1).Info("processing units need to be changed", "desiredProcessingUnits", desiredProcessingUnits, "sa.Status", sa.Status)

	if !r.needUpdateProcessingUnits(&sa, desiredProcessingUnits, now) {
		return ctrlreconcile.Result{}, nil
	}

	if err := s.UpdateInstance(ctx, desiredProcessingUnits); err != nil {
		r.recorder.Event(&sa, corev1.EventTypeWarning, "FailedUpdateInstance", err.Error())
		log.Error(err, "failed to update spanner instance status")
		return ctrlreconcile.Result{}, err
	}

	r.recorder.Eventf(&sa, corev1.EventTypeNormal, "Updated", "Updated processing units of %s/%s from %d to %d", sa.Spec.TargetInstance.ProjectID, sa.Spec.TargetInstance.InstanceID,
		normalizeProcessingUnitsOrNodes(sa.Status.CurrentProcessingUnits, sa.Status.CurrentNodes, sa.Spec.ScaleConfig.ComputeType), desiredProcessingUnits)

	log.Info("updated nodes via google cloud api", "before", normalizeProcessingUnitsOrNodes(sa.Status.CurrentProcessingUnits, sa.Status.CurrentNodes, sa.Spec.ScaleConfig.ComputeType), "after", desiredProcessingUnits)

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

// TODO: convert all internal computations to processing units only
func normalizeProcessingUnitsOrNodes(pu, nodes int, computeType spannerv1beta1.ComputeType) int {
	switch computeType {
	case spannerv1beta1.ComputeTypePU:
		return pu
	case spannerv1beta1.ComputeTypeNode:
		return nodes * 1000
	default:
		return -1
	}
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

func (r *SpannerAutoscalerReconciler) startSyncer(ctx context.Context, nn types.NamespacedName, projectID, instanceID string, credentials *syncer.Credentials) error {
	log := logging.FromContext(ctx)

	s, err := syncer.New(ctx, r.ctrlClient, nn, projectID, instanceID, credentials, r.recorder, syncer.WithLog(log))
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

func (r *SpannerAutoscalerReconciler) needCalcProcessingUnits(sa *spannerv1beta1.SpannerAutoscaler) bool {
	log := r.log

	switch {
	// TODO: Fix this to use only processing units
	case sa.Status.CurrentProcessingUnits == 0 && sa.Status.CurrentNodes == 0:
		log.Info("current processing units have not fetched yet")
		return false

	case sa.Status.InstanceState != spanner.StateReady:
		log.Info("instance state is not ready")
		return false
	default:
		return true
	}
}

func (r *SpannerAutoscalerReconciler) needUpdateProcessingUnits(sa *spannerv1beta1.SpannerAutoscaler, desiredProcessingUnits int, now time.Time) bool {
	log := r.log

	currentProcessingUnits := normalizeProcessingUnitsOrNodes(sa.Status.CurrentProcessingUnits, sa.Status.CurrentNodes, sa.Spec.ScaleConfig.ComputeType)

	switch {
	case desiredProcessingUnits == currentProcessingUnits:
		log.V(0).Info("the desired number of processing units is equal to that of the current; no need to scale")
		return false

	case desiredProcessingUnits > currentProcessingUnits && r.clock.Now().Before(sa.Status.LastScaleTime.Time.Add(10*time.Second)):
		log.Info("too short to scale up since instance scaled last",
			"now", now.String(),
			"last scale time", sa.Status.LastScaleTime,
		)

		return false

	case desiredProcessingUnits < currentProcessingUnits && r.clock.Now().Before(sa.Status.LastScaleTime.Time.Add(r.scaleDownInterval)):
		log.Info("too short to scale down since instance scaled nodes last",
			"now", now.String(),
			"last scale time", sa.Status.LastScaleTime,
		)

		return false

	default:
		return true
	}
}

// For testing purpose only
func calcDesiredNodes(currentCPU, currentNodes, targetCPU, minNodes, maxNodes, scaledownStepSize int) int {
	return calcDesiredProcessingUnits(currentCPU, currentNodes*1000, targetCPU, minNodes*1000, maxNodes*1000, scaledownStepSize) / 1000
}

// nextValidProcessingUnits finds next valid value in processing units.
// https://cloud.google.com/spanner/docs/compute-capacity?hl=en
// Valid values are
// If processingUnits < 1000, processing units must be multiples of 100.
// If processingUnits >= 1000, processing units must be multiples of 1000.
func nextValidProcessingUnits(processingUnits int) int {
	if processingUnits < 1000 {
		return ((processingUnits / 100) + 1) * 100
	}
	return ((processingUnits / 1000) + 1) * 1000
}

func maxInt(first int, rest ...int) int {
	result := first
	for _, v := range rest {
		if result < v {
			result = v
		}
	}
	return result
}

// calcDesiredProcessingUnits calculates the values needed to keep CPU utilization below TargetCPU.
func calcDesiredProcessingUnits(currentCPU, currentProcessingUnits, targetCPU, minProcessingUnits, maxProcessingUnits, scaledownStepSize int) int {
	totalCPUProduct1000 := currentCPU * currentProcessingUnits

	desiredProcessingUnits := maxInt(nextValidProcessingUnits(totalCPUProduct1000/targetCPU), currentProcessingUnits-scaledownStepSize*1000)

	switch {
	case desiredProcessingUnits < minProcessingUnits:
		return minProcessingUnits

	case desiredProcessingUnits > maxProcessingUnits:
		return maxProcessingUnits

	default:
		return desiredProcessingUnits
	}
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
