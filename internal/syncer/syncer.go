package syncer

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/option"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	utilclock "k8s.io/utils/clock"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	"github.com/mercari/spanner-autoscaler/internal/metrics"
	"github.com/mercari/spanner-autoscaler/internal/observability"
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
	UpdateInstance(ctx context.Context, desiredProcessingUnits int) (int, error)
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
		cred, err := google.CredentialsFromJSONWithTypeAndParams(
			ctx,
			c.ServiceAccountJSON,
			google.ServiceAccount,
			google.CredentialsParams{Scopes: []string{cloudPlatformScope}},
		)
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
	// projectID and instanceID are cached so observability metrics can be
	// emitted without an extra ctrlClient.Get of the SpannerAutoscaler on
	// every Cloud Monitoring fetch.
	projectID  string
	instanceID string
	interval   time.Duration

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
	projectID, instanceID string,
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
		projectID:      projectID,
		instanceID:     instanceID,
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

// labels returns the observability identity labels for this syncer's target.
func (s *syncer) labels() observability.Labels {
	return observability.LabelsFor(s.namespacedName, s.projectID, s.instanceID)
}

// UpdateInstance updates the target Spanner instance with the desired number of processing units.
func (s *syncer) UpdateInstance(ctx context.Context, desiredProcessingUnits int) (int, error) {
	err := s.updateInstanceAttempt(ctx, desiredProcessingUnits)
	if err == nil {
		return desiredProcessingUnits, nil
	}
	if status.Code(err) != codes.ResourceExhausted {
		return 0, err
	}

	originalErr := err
	fallbackPU, ok := s.quotaFallbackProcessingUnits(ctx, desiredProcessingUnits)
	if !ok {
		return 0, originalErr
	}

	err = s.updateInstanceAttempt(ctx, fallbackPU)
	if err != nil {
		return 0, err
	}

	return fallbackPU, nil
}

func (s *syncer) updateInstanceAttempt(ctx context.Context, desiredProcessingUnits int) error {
	start := s.clock.Now()
	err := s.spannerClient.UpdateInstance(ctx, &spanner.Instance{
		ProcessingUnits: desiredProcessingUnits,
	})
	observability.RecordInstanceUpdate(s.labels(), s.clock.Now().Sub(start), err)
	return err
}

func (s *syncer) quotaFallbackProcessingUnits(ctx context.Context, desiredProcessingUnits int) (int, bool) {
	log := s.log.WithValues("desiredProcessingUnits", desiredProcessingUnits)

	instance, err := s.spannerClient.GetInstance(ctx)
	if err != nil {
		s.skipQuotaFallback(log, "Skipped quota fallback because Spanner instance lookup failed",
			err, observability.QuotaLookupResultError)
		return 0, false
	}

	quota, err := s.metricsClient.GetQuota(ctx, instance.Config, s.clock.Now())
	if err != nil {
		// Unsupported instance configurations are recorded as "skipped" rather
		// than "error" because the lookup never actually attempted Monitoring.
		result := observability.QuotaLookupResultError
		msg := "Skipped quota fallback because Spanner quota lookup failed"
		if errors.Is(err, metrics.ErrUnsupportedInstanceConfig) {
			result = observability.QuotaLookupResultSkipped
			msg = "Skipped quota fallback because Spanner instance configuration is unsupported"
		}
		s.skipQuotaFallback(log, msg, err, result,
			"instanceConfig", instance.Config,
			"currentProcessingUnits", instance.ProcessingUnits,
		)
		return 0, false
	}
	observability.RecordQuotaLookup(s.labels(), observability.QuotaLookupResultSuccess, observability.QuotaLookupReasonOK)

	// quotaLog binds the instance + quota context so the two trailing log
	// calls stay aligned on which fields they expose.
	quotaLog := log.WithValues(
		"instanceConfig", instance.Config,
		"currentProcessingUnits", instance.ProcessingUnits,
		"quotaLimitNodes", quota.LimitNodes,
		"quotaUsageNodes", quota.UsageNodes,
		"quotaMetric", quota.QuotaMetric,
		"quotaLocation", quota.Location,
	)

	fallbackPU, ok := fallbackProcessingUnits(desiredProcessingUnits, instance.ProcessingUnits, quota)
	if !ok {
		quotaLog.Info("Skipped quota fallback retry")
		return 0, false
	}

	quotaLog.Info("Retrying Spanner instance update with quota fallback",
		"fallbackProcessingUnits", fallbackPU)
	return fallbackPU, true
}

// skipQuotaFallback records a quota_lookup_total{result, reason} sample and
// emits a single Info log with the error and any caller-supplied key/value
// pairs. Centralized so the three skip branches in quotaFallbackProcessingUnits
// stay aligned on which fields they expose.
func (s *syncer) skipQuotaFallback(log logr.Logger, msg string, err error, result string, extra ...any) {
	reason := quotaLookupReason(err)
	kvs := append([]any{"error", err, "reason", reason}, extra...)
	log.Info(msg, kvs...)
	observability.RecordQuotaLookup(s.labels(), result, reason)
}

func fallbackProcessingUnits(desiredProcessingUnits, currentProcessingUnits int, quota *metrics.Quota) (int, bool) {
	if quota == nil || quota.LimitNodes <= 0 || quota.UsageNodes < 0 || currentProcessingUnits <= 0 {
		return 0, false
	}

	// Convert PU → nodes with ceil so a 100 PU target is still counted as 1 node.
	currentTargetQuotaNodes := int64((currentProcessingUnits + 999) / 1000)
	availableNodesForTarget := quota.LimitNodes - (quota.UsageNodes - currentTargetQuotaNodes)
	if availableNodesForTarget <= 0 {
		return 0, false
	}

	availablePUForTarget := int(availableNodesForTarget) * 1000
	fallbackPU := desiredProcessingUnits
	if fallbackPU > availablePUForTarget {
		fallbackPU = availablePUForTarget
	}
	if fallbackPU <= currentProcessingUnits || fallbackPU >= desiredProcessingUnits {
		return 0, false
	}
	return fallbackPU, true
}

func quotaLookupReason(err error) string {
	switch {
	case errors.Is(err, metrics.ErrUnsupportedInstanceConfig):
		return observability.QuotaLookupReasonUnsupportedInstanceConfig
	case errors.Is(err, metrics.ErrQuotaNoData):
		return observability.QuotaLookupReasonNoData
	case errors.Is(err, metrics.ErrQuotaMalformedResponse):
		return observability.QuotaLookupReasonMalformedResponse
	case errors.Is(err, context.DeadlineExceeded):
		return observability.QuotaLookupReasonTimeout
	}

	switch status.Code(err) {
	case codes.PermissionDenied:
		return observability.QuotaLookupReasonPermissionDenied
	case codes.Unauthenticated:
		return observability.QuotaLookupReasonUnauthenticated
	case codes.Unavailable:
		return observability.QuotaLookupReasonUnavailable
	case codes.ResourceExhausted:
		return observability.QuotaLookupReasonResourceExhausted
	case codes.DeadlineExceeded:
		return observability.QuotaLookupReasonTimeout
	default:
		return observability.QuotaLookupReasonUnknown
	}
}

func (s *syncer) syncResource(ctx context.Context) error {
	log := s.log

	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	var sa spannerv1beta1.SpannerAutoscaler
	if err := s.ctrlClient.Get(ctx, s.namespacedName, &sa); err != nil {
		err = ctrlclient.IgnoreNotFound(err)
		if err != nil {
			s.recorder.Eventf(&sa, corev1.EventTypeWarning, "FailedGetClient", "%s", err.Error())
			log.Error(err, "unable to get spanner-autoscaler")
			return err
		}

		return nil
	}

	log.V(1).Info("resource status",
		"spannerautoscaler", sa,
	)

	metricFlags := sa.Spec.ScaleConfig.TargetCPUUtilization.ActiveMetricFlags()

	var instance *spanner.Instance
	var highPriorityMetrics, totalMetrics *metrics.InstanceMetrics
	var err error

	switch metricFlags {
	case spannerv1beta1.CPUMetricFlagHighPriority | spannerv1beta1.CPUMetricFlagTotal:
		instance, highPriorityMetrics, totalMetrics, err = s.getInstanceInfoDual(ctx)
	case spannerv1beta1.CPUMetricFlagTotal:
		var m *metrics.InstanceMetrics
		instance, m, err = s.getInstanceInfo(ctx, metrics.MetricTypeTotal)
		totalMetrics = m
	case spannerv1beta1.CPUMetricFlagHighPriority:
		var m *metrics.InstanceMetrics
		instance, m, err = s.getInstanceInfo(ctx, metrics.MetricTypeHighPriority)
		highPriorityMetrics = m
	default:
		// No metric configured — invalid spec that bypassed webhook validation. Skip sync.
		log.Info("skipping sync: no CPU metric threshold configured")
		return nil
	}
	if err != nil {
		s.recorder.Eventf(&sa, corev1.EventTypeWarning, "FailedSpannerAPICall", "%s", err.Error())
		log.Error(err, "unable to get instance info")
		return err
	}

	log.V(1).Info("spanner instance status",
		"currentProcessingUnits", instance.ProcessingUnits,
		"instanceState", instance.InstanceState,
		"currentCPUMetricType", metricFlags.ToCPUMetricType(),
		"currentHighPriorityCPUUtilization", func() int {
			if highPriorityMetrics != nil {
				return highPriorityMetrics.CurrentHighPriorityCPUUtilization
			}
			return 0
		}(),
		"currentTotalCPUUtilization", func() int {
			if totalMetrics != nil {
				return totalMetrics.CurrentTotalCPUUtilization
			}
			return 0
		}(),
	)

	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := s.ctrlClient.Get(ctx, s.namespacedName, &sa); err != nil {
			return err
		}
		sa.Status.CurrentProcessingUnits = instance.ProcessingUnits
		sa.Status.InstanceState = instance.InstanceState
		// Zero both CPU fields first, then populate based on mode.
		sa.Status.CurrentHighPriorityCPUUtilization = 0
		sa.Status.CurrentTotalCPUUtilization = 0
		switch metricFlags {
		case spannerv1beta1.CPUMetricFlagHighPriority | spannerv1beta1.CPUMetricFlagTotal:
			sa.Status.CurrentHighPriorityCPUUtilization = highPriorityMetrics.CurrentHighPriorityCPUUtilization
			sa.Status.CurrentTotalCPUUtilization = totalMetrics.CurrentTotalCPUUtilization
			sa.Status.CurrentCPUMetricType = spannerv1beta1.CPUMetricTypeBoth
		case spannerv1beta1.CPUMetricFlagTotal:
			sa.Status.CurrentTotalCPUUtilization = totalMetrics.CurrentTotalCPUUtilization
			sa.Status.CurrentCPUMetricType = spannerv1beta1.CPUMetricTypeTotal
		case spannerv1beta1.CPUMetricFlagHighPriority:
			sa.Status.CurrentHighPriorityCPUUtilization = highPriorityMetrics.CurrentHighPriorityCPUUtilization
			sa.Status.CurrentCPUMetricType = spannerv1beta1.CPUMetricTypeHighPriority
		}
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

// metricsTypeToCPUMetricType maps a metrics package MetricType to the
// corresponding v1beta1 CPUMetricType. Keeping this mapping in syncer avoids
// a cross-package import between metrics and v1beta1.
func metricsTypeToCPUMetricType(t metrics.MetricType) spannerv1beta1.CPUMetricType {
	switch t {
	case metrics.MetricTypeTotal:
		return spannerv1beta1.CPUMetricTypeTotal
	default: // metrics.MetricTypeHighPriority
		return spannerv1beta1.CPUMetricTypeHighPriority
	}
}

func (s *syncer) getInstanceInfo(ctx context.Context, metricType metrics.MetricType) (*spanner.Instance, *metrics.InstanceMetrics, error) {
	log := s.log
	eg, ctx := errgroup.WithContext(ctx)

	// Capture the base time once so the metrics query window is deterministic
	// even if the goroutine starts a few milliseconds later.
	now := s.clock.Now()

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
			"currentProcessingUnits", instance.ProcessingUnits,
			"instanceState", instance.InstanceState,
		)
		return nil
	})

	eg.Go(func() error {
		start := s.clock.Now()
		var err error
		instanceMetrics, err = s.metricsClient.GetInstanceMetrics(ctx, metricType, now)
		observability.RecordMetricsFetch(s.labels(), s.clock.Now().Sub(start), err)
		if err != nil {
			log.Error(err, "unable to get spanner instance metrics with client")
			return err
		}
		log.V(1).Info("successfully got spanner instance metrics with metrics client",
			"currentCPUMetricType", metricsTypeToCPUMetricType(metricType),
			"currentHighPriorityCPUUtilization", instanceMetrics.CurrentHighPriorityCPUUtilization,
			"currentTotalCPUUtilization", instanceMetrics.CurrentTotalCPUUtilization,
		)
		return nil
	})

	if err := eg.Wait(); err != nil {
		log.Error(err, "unable to get spanner instance status")
		return nil, nil, err
	}

	return instance, instanceMetrics, nil
}

// getInstanceInfoDual fetches the Spanner instance info and both CPU metrics concurrently.
// Used when both highPriority and total CPU targets are specified (dual CPU scaling mode).
func (s *syncer) getInstanceInfoDual(ctx context.Context) (*spanner.Instance, *metrics.InstanceMetrics, *metrics.InstanceMetrics, error) {
	log := s.log
	eg, ctx := errgroup.WithContext(ctx)

	// Share a single base time between the two metric queries so they always
	// target the same 60s alignment window. Without this, two independent
	// clock reads can land on different sides of a minute boundary and cause
	// the two CPU values to come from different aligned points.
	now := s.clock.Now()

	var (
		instance            *spanner.Instance
		highPriorityMetrics *metrics.InstanceMetrics
		totalMetrics        *metrics.InstanceMetrics
	)

	eg.Go(func() error {
		var err error
		instance, err = s.spannerClient.GetInstance(ctx)
		if err != nil {
			log.Error(err, "unable to get spanner instance with spanner client")
			return err
		}
		log.V(1).Info("successfully got spanner instance with spanner client",
			"currentProcessingUnits", instance.ProcessingUnits,
			"instanceState", instance.InstanceState,
		)
		return nil
	})

	eg.Go(func() error {
		start := s.clock.Now()
		var err error
		highPriorityMetrics, err = s.metricsClient.GetInstanceMetrics(ctx, metrics.MetricTypeHighPriority, now)
		observability.RecordMetricsFetch(s.labels(), s.clock.Now().Sub(start), err)
		if err != nil {
			log.Error(err, "unable to get high priority cpu metrics")
			return err
		}
		log.V(1).Info("successfully got high priority cpu metrics",
			"currentHighPriorityCPUUtilization", highPriorityMetrics.CurrentHighPriorityCPUUtilization,
		)
		return nil
	})

	eg.Go(func() error {
		start := s.clock.Now()
		var err error
		totalMetrics, err = s.metricsClient.GetInstanceMetrics(ctx, metrics.MetricTypeTotal, now)
		observability.RecordMetricsFetch(s.labels(), s.clock.Now().Sub(start), err)
		if err != nil {
			log.Error(err, "unable to get total cpu metrics")
			return err
		}
		log.V(1).Info("successfully got total cpu metrics",
			"currentTotalCPUUtilization", totalMetrics.CurrentTotalCPUUtilization,
		)
		return nil
	})

	if err := eg.Wait(); err != nil {
		log.Error(err, "unable to get spanner instance status")
		return nil, nil, nil, err
	}

	return instance, highPriorityMetrics, totalMetrics, nil
}
