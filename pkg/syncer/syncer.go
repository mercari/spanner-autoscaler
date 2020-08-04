package syncer

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"

	"k8s.io/client-go/tools/record"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/clock"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	spannerv1alpha1 "github.com/mercari/spanner-autoscaler/pkg/api/v1alpha1"
	"github.com/mercari/spanner-autoscaler/pkg/metrics"
	"github.com/mercari/spanner-autoscaler/pkg/spanner"
)

// Syncer represents a worker synchronizing a SpannerAutoscaler object status.
type Syncer interface {
	// Start starts synchronization of resource status.
	Start()
	// Stop stops synchronization of resource status.
	Stop()

	UpdateTarget(projectID, instanceID string, serviceAccountJSON []byte) bool

	UpdateInstance(ctx context.Context, desiredNodes int32) error
}

// syncer synchronizes SpannerAutoscalerStatus.
type syncer struct {
	projectID          string
	instanceID         string
	serviceAccountJSON []byte

	ctrlClient    ctrlclient.Client
	spannerClient spanner.Client
	metricsClient metrics.Client

	namespacedName types.NamespacedName
	interval       time.Duration

	stopCh chan struct{}

	clock clock.Clock
	log   logr.Logger

	recorder record.EventRecorder
}

var _ Syncer = (*syncer)(nil)

type Option func(*syncer)

func WithInterval(interval time.Duration) Option {
	return func(s *syncer) {
		s.interval = interval
	}
}

func WithLog(log logr.Logger) Option {
	return func(s *syncer) {
		s.log = log.WithName("syncer")
	}
}

func WithClock(clock clock.Clock) Option {
	return func(s *syncer) {
		s.clock = clock
	}
}

// New returns a new Syncer.
func New(
	ctx context.Context,
	ctrlClient ctrlclient.Client,
	namespacedName types.NamespacedName,
	projectID string,
	instanceID string,
	serviceAccountJSON []byte,
	recorder record.EventRecorder,
	opts ...Option,
) (Syncer, error) {
	spannerClient, err := spanner.NewClient(
		ctx,
		projectID,
		spanner.WithCredentialsJSON(serviceAccountJSON),
	)
	if err != nil {
		return nil, err
	}

	metricsClient, err := metrics.NewClient(
		ctx,
		projectID,
		metrics.WithCredentialsJSON(serviceAccountJSON),
	)
	if err != nil {
		return nil, err
	}

	s := &syncer{
		projectID:          projectID,
		instanceID:         instanceID,
		serviceAccountJSON: serviceAccountJSON,

		ctrlClient:     ctrlClient,
		spannerClient:  spannerClient,
		metricsClient:  metricsClient,
		namespacedName: namespacedName,
		interval:       time.Minute,
		stopCh:         make(chan struct{}),
		log:            zapr.NewLogger(zap.NewNop()),
		clock:          clock.RealClock{},
		recorder:       recorder,
	}

	for _, opt := range opts {
		opt(s)
	}

	return s, nil
}

// Start implements Syncer.
func (s *syncer) Start() {
	log := s.log.WithValues("interval", s.interval)

	log.V(1).Info("starting spanner-autoscaler")
	defer log.V(1).Info("shutting down spanner-autoscaler")

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			log.V(1).Info("ticker start")

			ctx := context.Background()
			if err := s.syncResource(ctx); err != nil {
				log.Error(err, "unable to sync resource")
			}
		case <-s.stopCh:
			log.V(1).Info("received stop signal")
			return
		}
	}
}

// Stop implements Syncer.
func (s *syncer) Stop() {
	close(s.stopCh)
}

// UpdateTarget updates target and returns wether did update or not.
func (s *syncer) UpdateTarget(projectID, instanceID string, serviceAccountJSON []byte) bool {
	updated := false

	if s.projectID != projectID {
		updated = true
		s.projectID = projectID
	}

	if s.instanceID != instanceID {
		updated = true
		s.instanceID = instanceID
	}

	if string(s.serviceAccountJSON) != string(serviceAccountJSON) {
		updated = true
		s.serviceAccountJSON = serviceAccountJSON
	}

	return updated
}

func (s *syncer) UpdateInstance(ctx context.Context, desiredNodes int32) error {
	err := s.spannerClient.UpdateInstance(ctx, s.instanceID, &spanner.Instance{
		NodeCount: &desiredNodes,
	})
	if err != nil {
		return err
	}

	return nil
}

func (s *syncer) syncResource(ctx context.Context) error {
	log := s.log

	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	var sa spannerv1alpha1.SpannerAutoscaler
	if err := s.ctrlClient.Get(ctx, s.namespacedName, &sa); err != nil {
		err = ctrlclient.IgnoreNotFound(err)
		if err != nil {
			s.recorder.Eventf(&sa, corev1.EventTypeWarning, "FailedGetClient", err.Error())
			log.Error(err, "unable to get spanner-autoscaler")
			return err
		}

		return nil
	}

	log.V(1).Info("resource status",
		"spannerautoscaler", sa,
	)

	instance, instanceMetrics, err := s.getInstanceInfo(ctx, *sa.Spec.ScaleTargetRef.InstanceID)
	if err != nil {
		s.recorder.Eventf(&sa, corev1.EventTypeWarning, "FailedSpannerAPICall", err.Error())
		log.Error(err, "unable to get instance info")
		return err
	}

	log.V(1).Info("spanner instance status",
		"current nodes", instance.NodeCount,
		"instance state", instance.InstanceState,
		"high priority cpu utilization", instanceMetrics.CurrentHighPriorityCPUUtilization,
	)

	saCopy := sa.DeepCopy()
	saCopy.Status.CurrentNodes = instance.NodeCount
	saCopy.Status.InstanceState = instance.InstanceState
	saCopy.Status.CurrentHighPriorityCPUUtilization = instanceMetrics.CurrentHighPriorityCPUUtilization
	saCopy.Status.LastSyncTime = &metav1.Time{Time: s.clock.Now()}

	if err := s.ctrlClient.Status().Update(ctx, saCopy); err != nil {
		s.recorder.Event(&sa, corev1.EventTypeWarning, "FailedUpdateStatus", err.Error())
		log.Error(err, "unable to update spanner status")
		return err
	}

	log.V(0).Info("updated spannerautoscaler resource")

	return nil
}

func (s *syncer) getInstanceInfo(ctx context.Context, instanceID string) (*spanner.Instance, *metrics.InstanceMetrics, error) {
	log := s.log
	eg, ctx := errgroup.WithContext(ctx)

	var (
		instance        *spanner.Instance
		instanceMetrics *metrics.InstanceMetrics
	)

	eg.Go(func() error {
		var err error
		instance, err = s.spannerClient.GetInstance(ctx, instanceID)
		if err != nil {
			log.Error(err, "unable to get spanner instance with spanner client")
			return err
		}
		log.V(1).Info("successfully got spanner instance with spanner client",
			"current nodes", instance.NodeCount,
			"instance state", instance.InstanceState,
		)
		return nil
	})

	eg.Go(func() error {
		var err error
		instanceMetrics, err = s.metricsClient.GetInstanceMetrics(ctx, instanceID)
		if err != nil {
			log.Error(err, "unable to get spanner instance metrics with client")
			return err
		}
		log.V(1).Info("successfully got spanner instance metrics with metrics client",
			"high priority cpu utilization", instanceMetrics.CurrentHighPriorityCPUUtilization,
		)
		return nil
	})

	if err := eg.Wait(); err != nil {
		log.Error(err, "unable to get spanner instance status")
		return nil, nil, err
	}

	return instance, instanceMetrics, nil
}
