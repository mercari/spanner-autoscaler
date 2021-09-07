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
	"k8s.io/apimachinery/pkg/util/clock"
	"k8s.io/client-go/tools/record"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	ctrlmanager "sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spannerv1alpha1 "github.com/mercari/spanner-autoscaler/pkg/api/v1alpha1"
	"github.com/mercari/spanner-autoscaler/pkg/logging"
	"github.com/mercari/spanner-autoscaler/pkg/pointer"
	"github.com/mercari/spanner-autoscaler/pkg/spanner"
	"github.com/mercari/spanner-autoscaler/pkg/syncer"
)

var (
	errFetchServiceAccountJSONNoNameSpecified   = errors.New("No name specified")
	errFetchServiceAccountJSONNoKeySpecified    = errors.New("No key specified")
	errFetchServiceAccountJSONNoSecretFound     = errors.New("No secret found by specified name")
	errFetchServiceAccountJSONNoSecretDataFound = errors.New("No secret found by specified key")
	errInvalidExclusiveCredentials              = errors.New("impersonateConfig and serviceAccountSecretRef are mutually exclusive")
)

const defaultMaxScaleDownNodes = 2

// SpannerAutoscalerReconciler reconciles a SpannerAutoscaler object.
type SpannerAutoscalerReconciler struct {
	ctrlClient ctrlclient.Client
	apiReader  ctrlclient.Reader

	scheme   *runtime.Scheme
	recorder record.EventRecorder

	syncers map[types.NamespacedName]syncer.Syncer

	scaleDownInterval time.Duration

	clock clock.Clock
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

func WithClock(clock clock.Clock) Option {
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
		clock:             clock.RealClock{},
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

// Reconcile implements ctrlreconcile.Reconciler.
func (r *SpannerAutoscalerReconciler) Reconcile(ctx context.Context, req ctrlreconcile.Request) (ctrlreconcile.Result, error) {
	nn := req.NamespacedName
	log := r.log.WithValues("namespaced name", nn)

	r.mu.RLock()
	s, syncerExists := r.syncers[nn]
	r.mu.RUnlock()

	var sa spannerv1alpha1.SpannerAutoscaler
	if err := r.ctrlClient.Get(ctx, nn, &sa); err != nil {
		err = ctrlclient.IgnoreNotFound(err)
		if err != nil {
			log.Error(err, "failed to get spanner-autoscaler")
			return ctrlreconcile.Result{}, err
		}

		if syncerExists {
			s.Stop()
			r.mu.Lock()
			delete(r.syncers, nn)
			r.mu.Unlock()
			log.Info("stopped syncer")
		}

		return ctrlreconcile.Result{}, nil
	}

	if sa.Spec.MaxScaleDownNodes == nil || *sa.Spec.MaxScaleDownNodes == 0 {
		sa.Spec.MaxScaleDownNodes = pointer.Int32(defaultMaxScaleDownNodes)
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
		ctx = logging.WithContext(ctx, log)

		if err := r.startSyncer(ctx, nn, *sa.Spec.ScaleTargetRef.ProjectID, *sa.Spec.ScaleTargetRef.InstanceID, credentials); err != nil {
			r.recorder.Event(&sa, corev1.EventTypeWarning, "FailedStartSyncer", err.Error())
			log.Error(err, "failed to start syncer")
			return ctrlreconcile.Result{}, err
		}

		return ctrlreconcile.Result{}, nil
	}

	// If target spanner instance or service account have been changed, then just replace syncer.
	if s.UpdateTarget(*sa.Spec.ScaleTargetRef.ProjectID, *sa.Spec.ScaleTargetRef.InstanceID, credentials) {
		s.Stop()
		r.mu.Lock()
		delete(r.syncers, nn)
		r.mu.Unlock()

		if err := r.startSyncer(ctx, nn, *sa.Spec.ScaleTargetRef.ProjectID, *sa.Spec.ScaleTargetRef.InstanceID, credentials); err != nil {
			r.recorder.Event(&sa, corev1.EventTypeWarning, "FailedStartSyncer", err.Error())
			log.Error(err, "failed to start syncer")
			return ctrlreconcile.Result{}, err
		}

		log.Info("replaced syncer", "namespaced name", sa)
		return ctrlreconcile.Result{}, nil
	}

	if sa.Status.LastScaleTime == nil {
		sa.Status.LastScaleTime = &metav1.Time{}
	}

	if !r.needCalcProcessingUnits(&sa) {
		return ctrlreconcile.Result{}, nil
	}

	desiredProcessingUnits := calcDesiredProcessingUnits(
		*sa.Status.CurrentHighPriorityCPUUtilization,
		normalizeProcessingUnitsOrNodes(sa.Status.CurrentProcessingUnits, sa.Status.CurrentNodes),
		*sa.Spec.TargetCPUUtilization.HighPriority,
		normalizeProcessingUnitsOrNodes(sa.Spec.MinProcessingUnits, sa.Spec.MinNodes),
		normalizeProcessingUnitsOrNodes(sa.Spec.MaxProcessingUnits, sa.Spec.MaxNodes),
		*sa.Spec.MaxScaleDownNodes,
	)

	now := r.clock.Now()

	if !r.needUpdateProcessingUnits(&sa, desiredProcessingUnits, now) {
		return ctrlreconcile.Result{}, nil
	}

	if err := s.UpdateInstance(ctx, desiredProcessingUnits); err != nil {
		r.recorder.Event(&sa, corev1.EventTypeWarning, "FailedUpdateInstance", err.Error())
		log.Error(err, "failed to update spanner instance status")
		return ctrlreconcile.Result{}, err
	}

	r.recorder.Eventf(&sa, corev1.EventTypeNormal, "Updated", "Updated processing units of %s/%s from %d to %d", *sa.Spec.ScaleTargetRef.ProjectID, *sa.Spec.ScaleTargetRef.InstanceID,
		normalizeProcessingUnitsOrNodes(sa.Status.CurrentProcessingUnits, sa.Status.CurrentNodes), desiredProcessingUnits)

	log.Info("updated nodes via google cloud api", "before", normalizeProcessingUnitsOrNodes(sa.Status.CurrentProcessingUnits, sa.Status.CurrentNodes), "after", desiredProcessingUnits)

	saCopy := sa.DeepCopy()
	saCopy.Status.DesiredProcessingUnits = &desiredProcessingUnits
	saCopy.Status.DesiredNodes = pointer.Int32(desiredProcessingUnits / 1000)
	saCopy.Status.LastScaleTime = &metav1.Time{Time: now}

	if err = r.ctrlClient.Status().Update(ctx, saCopy); err != nil {
		r.recorder.Event(&sa, corev1.EventTypeWarning, "FailedUpdateStatus", err.Error())
		log.Error(err, "failed to update spanner autoscaler status")
		return ctrlreconcile.Result{}, err
	}

	return ctrlreconcile.Result{}, nil
}

func normalizeProcessingUnitsOrNodes(processingUnits, nodes *int32) int32 {
	if processingUnits != nil {
		return *processingUnits
	}
	if nodes != nil {
		return *nodes * 1000
	}
	return 0
}

// SetupWithManager sets up the controller with ctrlmanager.Manager.
func (r *SpannerAutoscalerReconciler) SetupWithManager(mgr ctrlmanager.Manager) error {
	opts := ctrlcontroller.Options{
		Reconciler: r,
	}

	return ctrlbuilder.ControllerManagedBy(mgr).
		For(&spannerv1alpha1.SpannerAutoscaler{}).
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

func (r *SpannerAutoscalerReconciler) needCalcProcessingUnits(sa *spannerv1alpha1.SpannerAutoscaler) bool {
	log := r.log

	switch {
	case sa.Status.CurrentProcessingUnits == nil && sa.Status.CurrentNodes == nil:
		log.Info("current processing units have not fetched yet")
		return false

	case sa.Status.InstanceState != spanner.StateReady:
		log.Info("instance state is not ready")
		return false
	default:
		return true
	}
}

func (r *SpannerAutoscalerReconciler) needUpdateProcessingUnits(sa *spannerv1alpha1.SpannerAutoscaler, desiredProcessingUnits int32, now time.Time) bool {
	log := r.log

	currentProcessingUnits := normalizeProcessingUnitsOrNodes(sa.Status.CurrentProcessingUnits, sa.Status.CurrentNodes)

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
func calcDesiredNodes(currentCPU, currentNodes, targetCPU, minNodes, maxNodes, maxScaleDownNodes int32) int32 {
	return calcDesiredProcessingUnits(currentCPU, currentNodes*1000, targetCPU, minNodes*1000, maxNodes*1000, maxScaleDownNodes) / 1000
}

// nextValidProcessingUnits finds next valid value in processing units.
// https://cloud.google.com/spanner/docs/compute-capacity?hl=en
// Valid values are
// If processingUnits < 1000, processing units must be multiples of 100.
// If processingUnits >= 1000, processing units must be multiples of 1000.
func nextValidProcessingUnits(processingUnits int32) int32 {
	if processingUnits < 1000 {
		return ((processingUnits / 100) + 1) * 100
	}
	return ((processingUnits / 1000) + 1) * 1000
}

func maxInt32(first int32, rest ...int32) int32 {
	result := first
	for _, v := range rest {
		if result < v {
			result = v
		}
	}
	return result
}

// calcDesiredProcessingUnits calculates the values needed to keep CPU utilization below TargetCPU.
func calcDesiredProcessingUnits(currentCPU, currentProcessingUnits, targetCPU, minProcessingUnits, maxProcessingUnits, maxScaleDownNodes int32) int32 {
	totalCPUProduct1000 := currentCPU * currentProcessingUnits

	desiredProcessingUnits := maxInt32(nextValidProcessingUnits(totalCPUProduct1000/targetCPU), currentProcessingUnits-maxScaleDownNodes*1000)

	switch {
	case desiredProcessingUnits < minProcessingUnits:
		return minProcessingUnits

	case desiredProcessingUnits > maxProcessingUnits:
		return maxProcessingUnits

	default:
		return desiredProcessingUnits
	}
}

func (r *SpannerAutoscalerReconciler) fetchCredentials(ctx context.Context, sa *spannerv1alpha1.SpannerAutoscaler) (*syncer.Credentials, error) {
	secretRef := sa.Spec.ServiceAccountSecretRef
	impersonateConfig := sa.Spec.ImpersonateConfig

	if secretRef != nil && impersonateConfig != nil {
		return nil, errInvalidExclusiveCredentials
	}

	if secretRef != nil {
		if secretRef.Name == nil || *secretRef.Name == "" {
			return nil, errFetchServiceAccountJSONNoNameSpecified
		}

		if secretRef.Key == nil || *secretRef.Key == "" {
			return nil, errFetchServiceAccountJSONNoKeySpecified
		}

		var namespace string
		if secretRef.Namespace == nil || *secretRef.Namespace == "" {
			namespace = sa.Namespace
		} else {
			namespace = *secretRef.Namespace
		}

		var secret corev1.Secret
		key := ctrlclient.ObjectKey{
			Name:      *secretRef.Name,
			Namespace: namespace,
		}
		if err := r.apiReader.Get(ctx, key, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, errFetchServiceAccountJSONNoSecretFound
			}
			return nil, err
		}

		serviceAccountJSON, ok := secret.Data[*secretRef.Key]
		if !ok {
			return nil, errFetchServiceAccountJSONNoSecretDataFound
		}

		return syncer.NewServiceAccountJSONCredentials(serviceAccountJSON), nil
	}

	if impersonateConfig != nil {
		return syncer.NewServiceAccountImpersonate(impersonateConfig.TargetServiceAccount, impersonateConfig.Delegates), nil
	}

	return syncer.NewADCCredentials(), nil
}
