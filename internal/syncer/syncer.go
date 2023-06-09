package syncer

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/option"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	utilclock "k8s.io/utils/clock"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	"github.com/mercari/spanner-autoscaler/internal/metrics"
	"github.com/mercari/spanner-autoscaler/internal/spanner"
	"google.golang.org/api/impersonate"
)

// Syncer represents a worker synchronizing a SpannerAutoscaler object status.
type Syncer interface {
	// Start starts synchronization of resource status.
	Start()
	// Stop stops synchronization of resource status.
	Stop()

	// HasCredentials checks whether the existing credentials of the syncer, match the provided one or not
	HasCredentials(credentials *Credentials) bool
	UpdateInstance(ctx context.Context, desiredProcessingUnits int) error
}

type CredentialsType int

const (
	CredentialsTypeADC CredentialsType = iota
	CredentialsTypeServiceAccountJSON
	CredentialsTypeImpersonation
)

type ImpersonateConfig struct {
	TargetServiceAccount string
	Delegates            []string
}

type Credentials struct {
	Type               CredentialsType
	ServiceAccountJSON []byte
	ImpersonateConfig  *ImpersonateConfig
}

func NewADCCredentials() *Credentials {
	return &Credentials{Type: CredentialsTypeADC}
}

func NewServiceAccountJSONCredentials(json []byte) *Credentials {
	return &Credentials{Type: CredentialsTypeServiceAccountJSON, ServiceAccountJSON: json}
}

func NewServiceAccountImpersonate(targetServiceAccount string, delegates []string) *Credentials {
	return &Credentials{Type: CredentialsTypeImpersonation, ImpersonateConfig: &ImpersonateConfig{TargetServiceAccount: targetServiceAccount, Delegates: delegates}}
}

const cloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"

var (
	baseTokenSourceOnce sync.Once
	baseTokenSource     oauth2.TokenSource
	baseTokenSourceErr  error
)

// To reduce request to GKE metadata server, the base token source is reused across syncers.
// Note: Initialization is deferred because there are possible to use serviceAccountSecretRef with no available default token source.
func initializedBaseTokenSource() (oauth2.TokenSource, error) {
	baseTokenSourceOnce.Do(func() {
		baseTokenSource, baseTokenSourceErr = google.DefaultTokenSource(context.Background(), cloudPlatformScope)
	})
	return baseTokenSource, baseTokenSourceErr
}

// TokenSource create oauth2.TokenSource for Credentials.
// Note: We can specify scopes needed for spanner-autoscaler but it does increase maintenance cost.
// We should already use least privileged Google Service Accounts so it use cloudPlatformScope.
func (c *Credentials) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	switch c.Type {
	case CredentialsTypeADC:
		return initializedBaseTokenSource()
	case CredentialsTypeServiceAccountJSON:
		cred, err := google.CredentialsFromJSON(ctx, c.ServiceAccountJSON, cloudPlatformScope)
		if err != nil {
			return nil, err
		}
		return cred.TokenSource, nil
	case CredentialsTypeImpersonation:
		baseTS, err := initializedBaseTokenSource()
		if err != nil {
			return nil, err
		}
		ts, err := impersonate.CredentialsTokenSource(ctx, impersonate.CredentialsConfig{
			TargetPrincipal: c.ImpersonateConfig.TargetServiceAccount,
			Delegates:       c.ImpersonateConfig.Delegates,
			Scopes:          []string{cloudPlatformScope},
		},
			option.WithTokenSource(baseTS),
		)
		return ts, err
	default:
		return nil, fmt.Errorf("credentials type unknown: %v", c.Type)
	}
}

// syncer synchronizes SpannerAutoscalerStatus.
type syncer struct {
	credentials *Credentials

	ctrlClient    ctrlclient.Client
	spannerClient spanner.Client
	metricsClient metrics.Client

	namespacedName types.NamespacedName
	interval       time.Duration

	stopCh chan struct{}

	clock utilclock.Clock
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

func WithClock(clock utilclock.Clock) Option {
	return func(s *syncer) {
		s.clock = clock
	}
}

// New returns a new Syncer.
func New(
	ctx context.Context,
	ctrlClient ctrlclient.Client,
	namespacedName types.NamespacedName,
	credentials *Credentials,
	recorder record.EventRecorder,
	spannerClient spanner.Client,
	metricsClient metrics.Client,
	opts ...Option,
) (Syncer, error) {
	s := &syncer{
		credentials: credentials,

		ctrlClient:     ctrlClient,
		spannerClient:  spannerClient,
		metricsClient:  metricsClient,
		namespacedName: namespacedName,
		interval:       time.Minute,
		stopCh:         make(chan struct{}),
		log:            logr.Discard(),
		clock:          utilclock.RealClock{},
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

// HasCredentials checks whether the existing credentials of the syncer, match the provided one or not
func (s *syncer) HasCredentials(credentials *Credentials) bool {
	// TODO: Consider deepCopy
	return reflect.DeepEqual(s.credentials, credentials)
}

// UpdateInstance updates the target Spanner instance withe the desired number of processing units
func (s *syncer) UpdateInstance(ctx context.Context, desiredProcessingUnits int) error {
	err := s.spannerClient.UpdateInstance(ctx, &spanner.Instance{
		ProcessingUnits: desiredProcessingUnits,
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

	var sa spannerv1beta1.SpannerAutoscaler
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

	instance, instanceMetrics, err := s.getInstanceInfo(ctx)
	if err != nil {
		s.recorder.Eventf(&sa, corev1.EventTypeWarning, "FailedSpannerAPICall", err.Error())
		log.Error(err, "unable to get instance info")
		return err
	}

	log.V(1).Info("spanner instance status",
		"current processing untis", instance.ProcessingUnits,
		"instance state", instance.InstanceState,
		"high priority cpu utilization", instanceMetrics.CurrentHighPriorityCPUUtilization,
	)

	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := s.ctrlClient.Get(ctx, s.namespacedName, &sa); err != nil {
			return err
		}
		sa.Status.CurrentProcessingUnits = instance.ProcessingUnits
		sa.Status.InstanceState = instance.InstanceState
		sa.Status.CurrentHighPriorityCPUUtilization = instanceMetrics.CurrentHighPriorityCPUUtilization
		sa.Status.LastSyncTime = metav1.Time{Time: s.clock.Now()}

		return s.ctrlClient.Status().Update(ctx, &sa)
	})
	if err != nil {
		// May be conflict if max retries were hit, or may be something unrelated
		// like permissions or a network error
		s.recorder.Event(&sa, corev1.EventTypeWarning, "FailedSyncStatus", err.Error())
		log.Error(err, "unable to sync spanner status")
		return err
	}

	log.Info("synced spannerautoscaler resource status")

	return nil
}

func (s *syncer) getInstanceInfo(ctx context.Context) (*spanner.Instance, *metrics.InstanceMetrics, error) {
	log := s.log
	eg, ctx := errgroup.WithContext(ctx)

	var (
		instance        *spanner.Instance
		instanceMetrics *metrics.InstanceMetrics
	)

	eg.Go(func() error {
		var err error
		instance, err = s.spannerClient.GetInstance(ctx)
		if err != nil {
			log.Error(err, "unable to get spanner instance with spanner client")
			return err
		}
		log.V(1).Info("successfully got spanner instance with spanner client",
			"current processing units", instance.ProcessingUnits,
			"instance state", instance.InstanceState,
		)
		return nil
	})

	eg.Go(func() error {
		var err error
		instanceMetrics, err = s.metricsClient.GetInstanceMetrics(ctx)
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
