package syncer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	testingclock "k8s.io/utils/clock/testing"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlenvtest "sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	"github.com/mercari/spanner-autoscaler/internal/metrics"
	"github.com/mercari/spanner-autoscaler/internal/observability"
	"github.com/mercari/spanner-autoscaler/internal/spanner"
)

var scheme = runtime.NewScheme()

func init() {
	spannerv1beta1.SchemeBuilder.Register(&spannerv1beta1.SpannerAutoscaler{}, &spannerv1beta1.SpannerAutoscalerList{})
	clientgoscheme.AddToScheme(scheme)
	spannerv1beta1.AddToScheme(scheme)
	apiextensionsv1.AddToScheme(scheme)
}

func Test_syncer_syncResource(t *testing.T) {
	var (
		fakeName           = "fake-spanner-autoscaler"
		fakeNamespace      = "fake-namespace"
		fakeNamespacedName = types.NamespacedName{
			Namespace: fakeNamespace,
			Name:      fakeName,
		}
		fakeInstanceID        = "fake-instance-id"
		fakeTime              = time.Date(2020, 4, 1, 0, 0, 0, 0, time.Local)
		fakeSpannerAutoscaler = &spannerv1beta1.SpannerAutoscaler{
			TypeMeta: metav1.TypeMeta{
				Kind:       "SpannerAutoscaler",
				APIVersion: "spanner.mercari.com/v1beta1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      fakeName,
				Namespace: fakeNamespace,
			},
			Spec: spannerv1beta1.SpannerAutoscalerSpec{
				TargetInstance: spannerv1beta1.TargetInstance{
					ProjectID:  "fake-project-id",
					InstanceID: fakeInstanceID,
				},
				Authentication: spannerv1beta1.Authentication{
					Type: spannerv1beta1.AuthTypeSA,
					IAMKeySecret: &spannerv1beta1.IAMKeySecret{
						Namespace: "",
						Name:      "fake-service-account-secret",
						Key:       "fake-service-account-key",
					},
				},
				ScaleConfig: spannerv1beta1.ScaleConfig{
					ComputeType: spannerv1beta1.ComputeTypeNode,
					Nodes: spannerv1beta1.ScaleConfigNodes{
						Min: 1,
						Max: 3,
					},
					ScaledownStepSize: intstr.FromInt(1000),
					TargetCPUUtilization: spannerv1beta1.TargetCPUUtilization{
						HighPriority: intPtr(50),
					},
				},
			},
			Status: spannerv1beta1.SpannerAutoscalerStatus{},
		}
	)

	tests := []struct {
		name           string
		fakeInstance   *spanner.Instance
		fakeMetrics    *metrics.InstanceMetrics
		targetResource *spannerv1beta1.SpannerAutoscaler
		want           *spannerv1beta1.SpannerAutoscaler
		wantErr        bool
	}{
		{
			name: "sync and update instance",
			fakeInstance: &spanner.Instance{
				ProcessingUnits: 3000,
				InstanceState:   spanner.StateReady,
			},
			fakeMetrics: &metrics.InstanceMetrics{
				CurrentHighPriorityCPUUtilization: 30,
			},
			targetResource: func() *spannerv1beta1.SpannerAutoscaler {
				o := fakeSpannerAutoscaler.DeepCopy()
				return o
			}(),
			want: func() *spannerv1beta1.SpannerAutoscaler {
				o := fakeSpannerAutoscaler.DeepCopy()
				o.Status.CurrentProcessingUnits = 3000
				o.Status.InstanceState = spannerv1beta1.InstanceStateReady
				o.Status.CurrentHighPriorityCPUUtilization = 30
				o.Status.CurrentCPUMetricType = spannerv1beta1.CPUMetricTypeHighPriority
				o.Status.LastSyncTime = metav1.Time{Time: fakeTime}
				return o
			}(),
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			te := ctrlenvtest.Environment{
				CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
			}
			cfg, err := te.Start()
			if err != nil {
				t.Fatalf("unable to start test environment: %v", err)
			}
			defer te.Stop()

			ctrlClient, err := ctrlclient.New(cfg, ctrlclient.Options{
				Scheme: scheme,
			})
			if err != nil {
				t.Fatalf("unable to new controller client: %v", err)
			}

			ctx := context.Background()

			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: fakeNamespace,
				},
			}

			if err := ctrlClient.Create(ctx, ns); err != nil {
				t.Fatalf("failed to create namespace: %s", err)
			}

			if err := ctrlClient.Create(ctx, tt.targetResource); err != nil {
				t.Fatalf("unable to create SpannerAutoscaler: %v", err)
			}
			// Update object status via ctrlclient.StatusWriter because ctrlclient.Create does not create an object including status.
			if err := ctrlClient.Status().Update(ctx, tt.targetResource); err != nil {
				t.Fatalf("unable to update SpannerAutoscaler status: %v", err)
			}

			s := &syncer{
				ctrlClient:     ctrlClient,
				spannerClient:  spanner.NewFakeClient(tt.fakeInstance),
				metricsClient:  metrics.NewFakeClient(tt.fakeMetrics),
				namespacedName: fakeNamespacedName,
				log: func() logr.Logger {
					l := zap.NewAtomicLevelAt(zap.DebugLevel)
					return ctrlzap.New(ctrlzap.Level(&l))
				}(),
				clock: testingclock.NewFakeClock(fakeTime),
			}

			if err := s.syncResource(ctx); (err != nil) != tt.wantErr {
				t.Errorf("syncResource() error = %v, wantErr %v", err, tt.wantErr)
			}

			var got spannerv1beta1.SpannerAutoscaler
			err = ctrlClient.Get(ctx, fakeNamespacedName, &got)
			if err != nil {
				t.Fatalf("unable to get SpannerAutoscaler resource: %v", err)
			}

			if diff := cmp.Diff(tt.want.Status, got.Status); diff != "" {
				t.Fatalf("(-wantInstance, +got)\n%s", diff)
			}
		})
	}
}

func Test_syncer_getInstanceInfo(t *testing.T) {
	tests := []struct {
		name                string
		fakeInstance        *spanner.Instance
		fakeMetrics         *metrics.InstanceMetrics
		wantInstance        *spanner.Instance
		wantInstanceMetrics *metrics.InstanceMetrics
		wantErr             bool
	}{
		{
			name: "get instance info",
			fakeInstance: &spanner.Instance{
				ProcessingUnits: 1000,
				InstanceState:   spanner.StateReady,
			},
			fakeMetrics: &metrics.InstanceMetrics{
				CurrentHighPriorityCPUUtilization: 50,
			},
			wantInstance: &spanner.Instance{
				ProcessingUnits: 1000,
				InstanceState:   spanner.StateReady,
			},
			wantInstanceMetrics: &metrics.InstanceMetrics{
				CurrentHighPriorityCPUUtilization: 50,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &syncer{
				spannerClient: spanner.NewFakeClient(tt.fakeInstance),
				metricsClient: metrics.NewFakeClient(tt.fakeMetrics),
				log: func() logr.Logger {
					l := zap.NewAtomicLevelAt(zap.DebugLevel)
					return ctrlzap.New(ctrlzap.Level(&l))
				}(),
				clock: testingclock.NewFakeClock(time.Date(2020, 4, 1, 0, 0, 0, 0, time.Local)),
			}

			ctx := context.Background()
			gotInstance, gotInstanceMetrics, err := s.getInstanceInfo(ctx, metrics.MetricTypeHighPriority)
			if (err != nil) != tt.wantErr {
				t.Errorf("getInstanceInfo() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if diff := cmp.Diff(gotInstance, tt.wantInstance); diff != "" {
				t.Errorf("getInstanceInfo() gotInstance = %v, wantInstance %v", gotInstance, tt.wantInstance)
			}
			if diff := cmp.Diff(gotInstanceMetrics, tt.wantInstanceMetrics); diff != "" {
				t.Errorf("getInstanceInfo() gotInstanceMetrics = %v, wantInstance %v", gotInstanceMetrics, tt.wantInstanceMetrics)
			}
		})
	}
}

func intPtr(i int) *int { return &i }

func Test_fallbackProcessingUnits(t *testing.T) {
	tests := []struct {
		name      string
		desired   int
		current   int
		quota     *metrics.Quota
		want      int
		wantRetry bool
	}{
		{
			name:    "new project 100 nodes quota",
			desired: 101000,
			current: 100,
			quota: &metrics.Quota{
				LimitNodes: 100,
				UsageNodes: 1,
			},
			want:      100000,
			wantRetry: true,
		},
		{
			name:    "other instances consume quota",
			desired: 45000,
			current: 35000,
			quota: &metrics.Quota{
				LimitNodes: 40,
				UsageNodes: 38,
			},
			want:      37000,
			wantRetry: true,
		},
		{
			name:    "no room above current",
			desired: 45000,
			current: 40000,
			quota: &metrics.Quota{
				LimitNodes: 40,
				UsageNodes: 40,
			},
			wantRetry: false,
		},
		{
			name:    "quota says requested should fit",
			desired: 40000,
			current: 35000,
			quota: &metrics.Quota{
				LimitNodes: 40,
				UsageNodes: 35,
			},
			wantRetry: false,
		},
		{
			name:      "nil quota guards against panic",
			desired:   45000,
			current:   35000,
			quota:     nil,
			wantRetry: false,
		},
		{
			name:    "limit nodes is zero",
			desired: 45000,
			current: 35000,
			quota: &metrics.Quota{
				LimitNodes: 0,
				UsageNodes: 0,
			},
			wantRetry: false,
		},
		{
			name:    "usage nodes is negative",
			desired: 45000,
			current: 35000,
			quota: &metrics.Quota{
				LimitNodes: 40,
				UsageNodes: -1,
			},
			wantRetry: false,
		},
		{
			name:    "current processing units is zero",
			desired: 45000,
			current: 0,
			quota: &metrics.Quota{
				LimitNodes: 40,
				UsageNodes: 0,
			},
			wantRetry: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := fallbackProcessingUnits(tt.desired, tt.current, tt.quota)
			if ok != tt.wantRetry {
				t.Fatalf("fallbackProcessingUnits() retry = %t, want %t", ok, tt.wantRetry)
			}
			if got != tt.want {
				t.Errorf("fallbackProcessingUnits() = %d, want %d", got, tt.want)
			}
		})
	}
}

// scriptedSpannerClient returns the next error in updateErrs on each
// UpdateInstance call. A nil entry means "succeed and apply the patch".
// GetInstance returns a fixed snapshot. Used by tests that need to drive
// syncer.UpdateInstance through specific error sequences.
type scriptedSpannerClient struct {
	mu              sync.Mutex
	config          string
	processingUnits int
	updateErrs      []error
	updateCalls     []int // captured ProcessingUnits per call, in order
}

func (c *scriptedSpannerClient) GetInstance(_ context.Context) (*spanner.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &spanner.Instance{
		ProcessingUnits: c.processingUnits,
		InstanceState:   spanner.StateReady,
		Config:          c.config,
	}, nil
}

func (c *scriptedSpannerClient) UpdateInstance(_ context.Context, instance *spanner.Instance) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.updateCalls = append(c.updateCalls, instance.ProcessingUnits)
	if len(c.updateErrs) == 0 {
		c.processingUnits = instance.ProcessingUnits
		return nil
	}
	err := c.updateErrs[0]
	c.updateErrs = c.updateErrs[1:]
	if err == nil {
		c.processingUnits = instance.ProcessingUnits
	}
	return err
}

// recordingQuotaMetricsClient counts GetQuota calls so tests can assert the
// quota lookup was (or was not) invoked. Always returns a fixed Quota.
type recordingQuotaMetricsClient struct {
	mu         sync.Mutex
	quotaCalls int
	quota      *metrics.Quota
	quotaErr   error
}

func (c *recordingQuotaMetricsClient) GetInstanceMetrics(_ context.Context, _ metrics.MetricType, _ time.Time) (*metrics.InstanceMetrics, error) {
	return &metrics.InstanceMetrics{}, nil
}

func (c *recordingQuotaMetricsClient) GetQuota(_ context.Context, _ string, _ time.Time) (*metrics.Quota, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.quotaCalls++
	if c.quotaErr != nil {
		return nil, c.quotaErr
	}
	return c.quota, nil
}

func newSyncerForUpdate(spannerClient spanner.Client, metricsClient metrics.Client) *syncer {
	return &syncer{
		spannerClient:  spannerClient,
		metricsClient:  metricsClient,
		namespacedName: types.NamespacedName{Namespace: "ns", Name: "n"},
		projectID:      "p",
		instanceID:     "i",
		log:            logr.Discard(),
		clock:          testingclock.NewFakeClock(time.Date(2020, 4, 1, 0, 0, 0, 0, time.UTC)),
	}
}

// Test_syncer_UpdateInstance_NonRetryableError covers the design's "失敗が
// codes.ResourceExhausted 以外なら、従来通り error を返す。quota lookup はしない"
// path: a PermissionDenied from UpdateInstance must short-circuit before any
// GetQuota / fallback retry happens.
func Test_syncer_UpdateInstance_NonRetryableError(t *testing.T) {
	sp := &scriptedSpannerClient{
		config:          "projects/p/instanceConfigs/regional-asia-northeast1",
		processingUnits: 100,
		updateErrs:      []error{status.Error(codes.PermissionDenied, "denied")},
	}
	mc := &recordingQuotaMetricsClient{}
	s := newSyncerForUpdate(sp, mc)

	applied, err := s.UpdateInstance(context.Background(), 1000)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("UpdateInstance err code = %v, want PermissionDenied; err=%v", status.Code(err), err)
	}
	if applied != 0 {
		t.Errorf("applied = %d, want 0", applied)
	}
	if mc.quotaCalls != 0 {
		t.Errorf("GetQuota called %d times, want 0", mc.quotaCalls)
	}
	if got := len(sp.updateCalls); got != 1 {
		t.Errorf("UpdateInstance attempts = %d, want 1 (no fallback)", got)
	}
}

// Test_syncer_UpdateInstance_FallbackRetryFails covers the design's "fallback
// 更新も失敗したら、その error を返す。再帰や複数回 retry はしない" path: the
// retry's error (not the original ResourceExhausted) propagates to the caller,
// and only two UpdateInstance attempts are made.
func Test_syncer_UpdateInstance_FallbackRetryFails(t *testing.T) {
	sp := &scriptedSpannerClient{
		config:          "projects/p/instanceConfigs/regional-asia-northeast1",
		processingUnits: 100,
		updateErrs: []error{
			status.Error(codes.ResourceExhausted, "quota"),
			status.Error(codes.Internal, "retry failed"),
		},
	}
	mc := &recordingQuotaMetricsClient{
		quota: &metrics.Quota{LimitNodes: 100, UsageNodes: 1},
	}
	s := newSyncerForUpdate(sp, mc)

	applied, err := s.UpdateInstance(context.Background(), 101000)
	if status.Code(err) != codes.Internal {
		t.Fatalf("UpdateInstance err code = %v, want Internal (retry error); err=%v", status.Code(err), err)
	}
	if applied != 0 {
		t.Errorf("applied = %d, want 0", applied)
	}
	if mc.quotaCalls != 1 {
		t.Errorf("GetQuota calls = %d, want 1", mc.quotaCalls)
	}
	if got := len(sp.updateCalls); got != 2 {
		t.Errorf("UpdateInstance attempts = %d, want 2 (initial + 1 retry)", got)
	}
	if got := sp.updateCalls[1]; got != 100000 {
		t.Errorf("fallback retry PU = %d, want 100000", got)
	}
}

func Test_quotaLookupReason(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"unsupported config", metrics.ErrUnsupportedInstanceConfig, observability.QuotaLookupReasonUnsupportedInstanceConfig},
		{"no data", metrics.ErrQuotaNoData, observability.QuotaLookupReasonNoData},
		{"malformed response", metrics.ErrQuotaMalformedResponse, observability.QuotaLookupReasonMalformedResponse},
		{"context deadline exceeded", context.DeadlineExceeded, observability.QuotaLookupReasonTimeout},
		{"grpc permission denied", status.Error(codes.PermissionDenied, "x"), observability.QuotaLookupReasonPermissionDenied},
		{"grpc unauthenticated", status.Error(codes.Unauthenticated, "x"), observability.QuotaLookupReasonUnauthenticated},
		{"grpc unavailable", status.Error(codes.Unavailable, "x"), observability.QuotaLookupReasonUnavailable},
		{"grpc resource exhausted", status.Error(codes.ResourceExhausted, "x"), observability.QuotaLookupReasonResourceExhausted},
		{"grpc deadline exceeded", status.Error(codes.DeadlineExceeded, "x"), observability.QuotaLookupReasonTimeout},
		{"plain error falls through to unknown", errors.New("boom"), observability.QuotaLookupReasonUnknown},
		{"wrapped sentinel still resolves", fmt.Errorf("outer: %w", metrics.ErrQuotaNoData), observability.QuotaLookupReasonNoData},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := quotaLookupReason(tt.err); got != tt.want {
				t.Errorf("quotaLookupReason(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

// recordingMetricsClient is a metrics.Client that records the now value each
// GetInstanceMetrics call received, keyed by MetricType. Used to verify that
// dual-mode metric queries share a single base time.
type recordingMetricsClient struct {
	mu    sync.Mutex
	calls map[metrics.MetricType]time.Time
}

func (c *recordingMetricsClient) GetInstanceMetrics(_ context.Context, metricType metrics.MetricType, now time.Time) (*metrics.InstanceMetrics, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.calls == nil {
		c.calls = map[metrics.MetricType]time.Time{}
	}
	c.calls[metricType] = now
	switch metricType {
	case metrics.MetricTypeTotal:
		return &metrics.InstanceMetrics{CurrentTotalCPUUtilization: 10}, nil
	default:
		return &metrics.InstanceMetrics{CurrentHighPriorityCPUUtilization: 20}, nil
	}
}

func (c *recordingMetricsClient) GetQuota(_ context.Context, _ string, _ time.Time) (*metrics.Quota, error) {
	return nil, metrics.ErrQuotaNoData
}

func Test_syncer_getInstanceInfoDual_sharesBaseTime(t *testing.T) {
	rec := &recordingMetricsClient{}
	fakeTime := time.Date(2020, 4, 1, 12, 34, 56, 0, time.UTC)
	s := &syncer{
		spannerClient: spanner.NewFakeClient(&spanner.Instance{
			ProcessingUnits: 1000,
			InstanceState:   spanner.StateReady,
		}),
		metricsClient: rec,
		log: func() logr.Logger {
			l := zap.NewAtomicLevelAt(zap.DebugLevel)
			return ctrlzap.New(ctrlzap.Level(&l))
		}(),
		clock: testingclock.NewFakeClock(fakeTime),
	}

	if _, _, _, err := s.getInstanceInfoDual(context.Background()); err != nil {
		t.Fatalf("getInstanceInfoDual() error: %v", err)
	}

	hp, okHP := rec.calls[metrics.MetricTypeHighPriority]
	tot, okTotal := rec.calls[metrics.MetricTypeTotal]
	if !okHP || !okTotal {
		t.Fatalf("expected both metric types to be queried, got calls=%v", rec.calls)
	}
	if !hp.Equal(tot) {
		t.Errorf("dual-mode calls received different now values: highPriority=%s total=%s", hp, tot)
	}
	if !hp.Equal(fakeTime) {
		t.Errorf("now passed to GetInstanceMetrics = %s, want %s", hp, fakeTime)
	}
}
