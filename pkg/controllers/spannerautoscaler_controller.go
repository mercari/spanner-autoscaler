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
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
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
)

const defaultMaxScaleDownNodes = 2

// SpannerAutoscalerReconciler reconciles a SpannerAutoscaler object.
type SpannerAutoscalerReconciler struct {
	ctrlClient ctrlclient.Client
	apiReader  ctrlclient.Reader

	scheme  *runtime.Scheme
	recoder record.EventRecorder

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
	opts ...Option,
) *SpannerAutoscalerReconciler {
	r := &SpannerAutoscalerReconciler{
		ctrlClient:        ctrlClient,
		apiReader:         apiReader,
		scheme:            scheme,
		recoder:           recorder,
		syncers:           make(map[types.NamespacedName]syncer.Syncer),
		scaleDownInterval: 55 * time.Minute,
		clock:             clock.RealClock{},
		log:               zapr.NewLogger(zap.NewNop()),
	}

	for _, opt := range opts {
		opt(r)
	}

	return r
}

// +kubebuilder:rbac:groups=spanner.mercari.com,resources=spannerautoscalers,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=spanner.mercari.com,resources=spannerautoscalers/status,verbs=get;update;patch

// Reconcile implements ctrlreconcile.Reconciler.
func (r *SpannerAutoscalerReconciler) Reconcile(req ctrlreconcile.Request) (ctrlreconcile.Result, error) {
	nn := req.NamespacedName
	log := r.log.WithValues("namespaced name", nn)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

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

	saj, err := r.fetchServiceAccountJSON(ctx, &sa)
	if err != nil {
		r.recoder.Event(&sa, corev1.EventTypeWarning, "ServiceAccountRequired", err.Error())
		log.Error(err, "failed to fetch service account")
		return ctrlreconcile.Result{}, err
	}

	// If the syncer does not exist, start a syncer.
	if !syncerExists {
		ctx = logging.WithContext(ctx, log)

		if err := r.startSyncer(ctx, nn, *sa.Spec.ScaleTargetRef.ProjectID, *sa.Spec.ScaleTargetRef.InstanceID, saj); err != nil {
			r.recoder.Event(&sa, corev1.EventTypeWarning, "FailedStartSyncer", err.Error())
			log.Error(err, "failed to start syncer")
			return ctrlreconcile.Result{}, err
		}

		return ctrlreconcile.Result{}, nil
	}

	// If target spanner instance or service account have been changed, then just update syncer.
	if s.UpdateTarget(*sa.Spec.ScaleTargetRef.ProjectID, *sa.Spec.ScaleTargetRef.InstanceID, saj) {
		log.Info("updated syncer", "namespaced name", sa)
		return ctrlreconcile.Result{}, nil
	}

	if sa.Status.LastScaleTime == nil {
		sa.Status.LastScaleTime = &metav1.Time{}
	}

	if !r.needCalcNodes(&sa) {
		return ctrlreconcile.Result{}, nil
	}

	desiredNodes := calcDesiredNodes(
		*sa.Status.CurrentHighPriorityCPUUtilization,
		*sa.Status.CurrentNodes,
		*sa.Spec.TargetCPUUtilization.HighPriority,
		*sa.Spec.MinNodes,
		*sa.Spec.MaxNodes,
		*sa.Spec.MaxScaleDownNodes,
	)

	if !r.needUpdateNodes(&sa, desiredNodes) {
		return ctrlreconcile.Result{}, nil
	}

	if err := s.UpdateInstance(ctx, desiredNodes); err != nil {
		r.recoder.Event(&sa, corev1.EventTypeWarning, "FailedUpdateInstance", err.Error())
		log.Error(err, "failed to update spanner instance status")
		return ctrlreconcile.Result{}, err
	}

	r.recoder.Eventf(&sa, corev1.EventTypeNormal, "Updated", "Updated number of nodes from %d to %d", *sa.Status.CurrentNodes, desiredNodes)

	log.Info("updated nodes via google cloud api", "before", *sa.Status.CurrentNodes, "after", desiredNodes)

	saCopy := sa.DeepCopy()
	saCopy.Status.DesiredNodes = &desiredNodes
	saCopy.Status.LastScaleTime = &metav1.Time{Time: r.clock.Now()}

	if err = r.ctrlClient.Status().Update(ctx, saCopy); err != nil {
		r.recoder.Event(&sa, corev1.EventTypeWarning, "FailedUpdateStatus", err.Error())
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
		For(&spannerv1alpha1.SpannerAutoscaler{}).
		WithOptions(opts).
		Complete(r)
}

func (r *SpannerAutoscalerReconciler) startSyncer(ctx context.Context, nn types.NamespacedName, projectID, instanceID string, serviceAccountJSON []byte) error {
	log := logging.FromContext(ctx)

	s, err := syncer.New(ctx, r.ctrlClient, nn, projectID, instanceID, serviceAccountJSON, syncer.WithLog(log))
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

func (r *SpannerAutoscalerReconciler) needCalcNodes(sa *spannerv1alpha1.SpannerAutoscaler) bool {
	log := r.log

	switch {
	case sa.Status.CurrentNodes == nil:
		log.Info("current nodes have not fetched yet")
		return false

	case sa.Status.InstanceState != spanner.StateReady:
		log.Info("instance state is not ready")
		return false
	default:
		return true
	}
}

func (r *SpannerAutoscalerReconciler) needUpdateNodes(sa *spannerv1alpha1.SpannerAutoscaler, desiredNodes int32) bool {
	log := r.log

	switch {
	case desiredNodes == *sa.Status.CurrentNodes:
		log.V(0).Info("the desired number of nodes is equal to that of the current; no need to scale nodes")
		return false

	case desiredNodes < *sa.Status.CurrentNodes && r.clock.Now().Before(sa.Status.LastScaleTime.Time.Add(r.scaleDownInterval)):
		log.Info("too short to scale down since instance scaled nodes last",
			"now", r.clock.Now().String(),
			"last scale time", sa.Status.LastScaleTime,
		)

		return false

	default:
		return true
	}
}

func calcDesiredNodes(currentCPU, currentNodes, targetCPU, minNodes, maxNodes, maxScaleDownNodes int32) int32 {
	totalCPU := currentCPU * currentNodes
	desiredNodes := totalCPU/targetCPU + 1 // roundup

	if (currentNodes - desiredNodes) > maxScaleDownNodes {
		desiredNodes = currentNodes - maxScaleDownNodes
	}

	switch {
	case desiredNodes < minNodes:
		return minNodes

	case desiredNodes > maxNodes:
		return maxNodes

	default:
		return desiredNodes
	}
}

func (r *SpannerAutoscalerReconciler) fetchServiceAccountJSON(ctx context.Context, sa *spannerv1alpha1.SpannerAutoscaler) ([]byte, error) {
	secretRef := sa.Spec.ServiceAccountSecretRef

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

	return serviceAccountJSON, nil
}
