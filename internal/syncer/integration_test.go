package syncer

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	monitoringpb "cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"github.com/go-logr/logr"
	dto "github.com/prometheus/client_model/go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"
	testingclock "k8s.io/utils/clock/testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/mercari/spanner-autoscaler/internal/metrics"
	"github.com/mercari/spanner-autoscaler/internal/monitoringemulator"
	"github.com/mercari/spanner-autoscaler/internal/observability"
	"github.com/mercari/spanner-autoscaler/internal/spanner"
	"github.com/mercari/spanner-autoscaler/internal/spanneremulator"
)

// integrationFixture wires the Spanner and Cloud Monitoring emulators behind
// localhost gRPC listeners and exposes their stores so each test can configure
// quotas, instances, and (optionally) Monitoring failure injection without
// reaching into the emulators' internals.
type integrationFixture struct {
	spannerSrv         *spanneremulator.Server
	quotaStore         *monitoringemulator.QuotaStore
	quotaModeStore     *monitoringemulator.QuotaModeStore
	spannerEndpoint    string
	monitoringEndpoint string
	registry           *prometheus.Registry
}

func newIntegrationFixture(t *testing.T) *integrationFixture {
	t.Helper()

	spannerSrv := spanneremulator.NewServer()
	spannerLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen spanner: %v", err)
	}
	spannerGrpc := grpc.NewServer()
	instancepb.RegisterInstanceAdminServer(spannerGrpc, spannerSrv)
	go func() { _ = spannerGrpc.Serve(spannerLis) }()

	staticStore := monitoringemulator.NewStaticStore()
	workloadStore := monitoringemulator.NewWorkloadStore()
	scenarioStore := monitoringemulator.NewScenarioStore()
	quotaStore := monitoringemulator.NewQuotaStore()
	quotaModeStore := monitoringemulator.NewQuotaModeStore()
	monitoringSrv := monitoringemulator.NewMetricServiceServer(staticStore, workloadStore, scenarioStore, quotaStore, quotaModeStore, nil)
	monLis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		spannerGrpc.GracefulStop()
		t.Fatalf("listen monitoring: %v", err)
	}
	monGrpc := grpc.NewServer()
	monitoringpb.RegisterMetricServiceServer(monGrpc, monitoringSrv)
	go func() { _ = monGrpc.Serve(monLis) }()

	t.Cleanup(func() {
		spannerGrpc.GracefulStop()
		monGrpc.GracefulStop()
	})

	registry := prometheus.NewRegistry()
	if err := observability.Register(registry); err != nil {
		t.Fatalf("observability.Register: %v", err)
	}

	return &integrationFixture{
		spannerSrv:         spannerSrv,
		quotaStore:         quotaStore,
		quotaModeStore:     quotaModeStore,
		spannerEndpoint:    spannerLis.Addr().String(),
		monitoringEndpoint: monLis.Addr().String(),
		registry:           registry,
	}
}

// makeSyncer builds a syncer that talks to the fixture's emulators. Each call
// registers a DeleteSeries cleanup so per-test metric state is discarded.
func (f *integrationFixture) makeSyncer(t *testing.T, projectID, instanceID, name string) (*syncer, observability.Labels) {
	t.Helper()
	ctx := context.Background()
	spClient, err := spanner.NewClient(ctx, projectID, instanceID, spanner.WithEndpoint(f.spannerEndpoint))
	if err != nil {
		t.Fatalf("spanner.NewClient: %v", err)
	}
	metricsClient, err := metrics.NewClient(ctx, projectID, instanceID, metrics.WithEndpoint(f.monitoringEndpoint))
	if err != nil {
		t.Fatalf("metrics.NewClient: %v", err)
	}
	nn := types.NamespacedName{Namespace: "ns", Name: name}
	s := &syncer{
		spannerClient:  spClient,
		metricsClient:  metricsClient,
		namespacedName: nn,
		projectID:      projectID,
		instanceID:     instanceID,
		log:            logr.Discard(),
		clock:          testingclock.NewFakeClock(time.Now()),
	}
	labels := observability.LabelsFor(nn, projectID, instanceID)
	t.Cleanup(func() { observability.DeleteSeries(labels) })
	return s, labels
}

// counterValue returns the current value of the named counter whose label set
// matches every key/value in want. Missing series read as 0, so a caller that
// expects "exactly 1" will see "0 → fail" if the Record* path was never taken.
func counterValue(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != name {
			continue
		}
		for _, m := range fam.GetMetric() {
			if labelsMatch(m.GetLabel(), want) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func labelsMatch(got []*dto.LabelPair, want map[string]string) bool {
	pairs := make(map[string]string, len(got))
	for _, lp := range got {
		pairs[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if pairs[k] != v {
			return false
		}
	}
	return true
}

func instanceName(projectID, instanceID string) string {
	return fmt.Sprintf("projects/%s/instances/%s", projectID, instanceID)
}

func configName(projectID, configID string) string {
	return fmt.Sprintf("projects/%s/instanceConfigs/%s", projectID, configID)
}

// TestIntegration_RegionalFallbackSuccess matches the design's regional
// fallback example: limit 100 nodes, usage 1 node, current 100 PU on this
// instance, requested 101000 PU → fallback to 100000 PU. Verifies that:
//   - applied PU is the fallback value (not the requested one)
//   - the Spanner emulator now holds 100000 PU
//   - the four metrics called out in the design each increment by 1
func TestIntegration_RegionalFallbackSuccess(t *testing.T) {
	f := newIntegrationFixture(t)

	const projectID, instanceID = "ip-regional", "inst-regional"
	const configID = "regional-asia-northeast1"
	f.spannerSrv.AdminUpsertInstance(instanceName(projectID, instanceID), 100, configName(projectID, configID))
	f.spannerSrv.AdminSetQuota(projectID, configID, 100000)
	f.quotaStore.Set(projectID, "asia-northeast1", monitoringemulator.QuotaEntry{
		QuotaMetric: "spanner.googleapis.com/nodes",
		LimitName:   "SpannerNodesPerProject",
		LimitNodes:  100,
		UsageNodes:  1,
	})

	s, labels := f.makeSyncer(t, projectID, instanceID, "regional")
	applied, err := s.UpdateInstance(context.Background(), 101000)
	if err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}
	if applied != 100000 {
		t.Errorf("applied = %d, want 100000", applied)
	}

	inst, err := s.spannerClient.GetInstance(context.Background())
	if err != nil {
		t.Fatalf("GetInstance after fallback: %v", err)
	}
	if inst.ProcessingUnits != 100000 {
		t.Errorf("instance ProcessingUnits = %d, want 100000", inst.ProcessingUnits)
	}

	wantIdentity := identityLabels(labels)
	if got := counterValue(t, f.registry, "spanner_autoscaler_instance_update_errors_total", merge(wantIdentity, map[string]string{"grpc_code": "resource_exhausted"})); got != 1 {
		t.Errorf("instance_update_errors_total{grpc_code=resource_exhausted} = %v, want 1", got)
	}
	if got := counterValue(t, f.registry, "spanner_autoscaler_instance_update_total", merge(wantIdentity, map[string]string{"result": "error"})); got != 1 {
		t.Errorf("instance_update_total{result=error} = %v, want 1", got)
	}
	if got := counterValue(t, f.registry, "spanner_autoscaler_instance_update_total", merge(wantIdentity, map[string]string{"result": "success"})); got != 1 {
		t.Errorf("instance_update_total{result=success} (fallback retry) = %v, want 1", got)
	}
	if got := counterValue(t, f.registry, "spanner_autoscaler_quota_lookup_total", merge(wantIdentity, map[string]string{"result": "success", "reason": "ok"})); got != 1 {
		t.Errorf("quota_lookup_total{result=success,reason=ok} = %v, want 1", got)
	}
}

// TestIntegration_MultiRegionFallbackSuccess covers the design's multi-region
// case where the config id is the bare base config (e.g. "nam6") and the
// matching quota metric is "spanner.googleapis.com/nodes_<config>". The
// location label is taken from whatever Monitoring returns (here "global"),
// not pre-derived from the config id.
func TestIntegration_MultiRegionFallbackSuccess(t *testing.T) {
	f := newIntegrationFixture(t)

	const projectID, instanceID = "ip-nam6", "inst-nam6"
	const configID = "nam6"
	f.spannerSrv.AdminUpsertInstance(instanceName(projectID, instanceID), 100, configName(projectID, configID))
	f.spannerSrv.AdminSetQuota(projectID, configID, 100000)
	f.quotaStore.Set(projectID, "global", monitoringemulator.QuotaEntry{
		QuotaMetric: "spanner.googleapis.com/nodes_nam6",
		LimitName:   "SpannerNodesPerNam6",
		LimitNodes:  100,
		UsageNodes:  1,
	})

	s, _ := f.makeSyncer(t, projectID, instanceID, "nam6")
	applied, err := s.UpdateInstance(context.Background(), 101000)
	if err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}
	if applied != 100000 {
		t.Errorf("applied = %d, want 100000", applied)
	}
}

// TestIntegration_DualRegionFallbackSuccess covers dual-region config ids
// whose hyphens get normalized to underscores in the candidate quota metric
// (dual-region-japan1 → spanner.googleapis.com/nodes_dual_region_japan1).
func TestIntegration_DualRegionFallbackSuccess(t *testing.T) {
	f := newIntegrationFixture(t)

	const projectID, instanceID = "ip-dual", "inst-dual"
	const configID = "dual-region-japan1"
	f.spannerSrv.AdminUpsertInstance(instanceName(projectID, instanceID), 100, configName(projectID, configID))
	f.spannerSrv.AdminSetQuota(projectID, configID, 100000)
	f.quotaStore.Set(projectID, "global", monitoringemulator.QuotaEntry{
		QuotaMetric: "spanner.googleapis.com/nodes_dual_region_japan1",
		LimitName:   "SpannerNodesPerDualRegionJapan1",
		LimitNodes:  100,
		UsageNodes:  1,
	})

	s, _ := f.makeSyncer(t, projectID, instanceID, "dual")
	applied, err := s.UpdateInstance(context.Background(), 101000)
	if err != nil {
		t.Fatalf("UpdateInstance: %v", err)
	}
	if applied != 100000 {
		t.Errorf("applied = %d, want 100000", applied)
	}
}

// TestIntegration_UnsupportedCustomConfig confirms that a "custom-*" config
// short-circuits the Monitoring lookup: the syncer records
// quota_lookup_total{result=skipped,reason=unsupported_instance_config},
// does not retry, and surfaces the original ResourceExhausted to the caller.
func TestIntegration_UnsupportedCustomConfig(t *testing.T) {
	f := newIntegrationFixture(t)

	const projectID, instanceID = "ip-custom", "inst-custom"
	const configID = "custom-foo"
	f.spannerSrv.AdminUpsertInstance(instanceName(projectID, instanceID), 100, configName(projectID, configID))
	f.spannerSrv.AdminSetQuota(projectID, configID, 100000)

	s, labels := f.makeSyncer(t, projectID, instanceID, "custom")
	applied, err := s.UpdateInstance(context.Background(), 101000)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("UpdateInstance err code = %v, want ResourceExhausted; err=%v", status.Code(err), err)
	}
	if applied != 0 {
		t.Errorf("applied = %d, want 0 (no retry)", applied)
	}

	inst, err := s.spannerClient.GetInstance(context.Background())
	if err != nil {
		t.Fatalf("GetInstance after failure: %v", err)
	}
	if inst.ProcessingUnits != 100 {
		t.Errorf("instance ProcessingUnits = %d, want 100 (unchanged)", inst.ProcessingUnits)
	}

	wantIdentity := identityLabels(labels)
	if got := counterValue(t, f.registry, "spanner_autoscaler_quota_lookup_total", merge(wantIdentity, map[string]string{"result": "skipped", "reason": "unsupported_instance_config"})); got != 1 {
		t.Errorf("quota_lookup_total{result=skipped,reason=unsupported_instance_config} = %v, want 1", got)
	}
	// No fallback retry, so the only UpdateInstance attempt is the failed one.
	if got := counterValue(t, f.registry, "spanner_autoscaler_instance_update_total", merge(wantIdentity, map[string]string{"result": "success"})); got != 0 {
		t.Errorf("instance_update_total{result=success} = %v, want 0", got)
	}
}

// TestIntegration_MonitoringPermissionDenied uses the quota-mode failure
// injection endpoint to make the Monitoring quota query return
// PermissionDenied. The syncer must record quota_lookup_total with the
// permission_denied reason and return the original ResourceExhausted instead
// of swallowing it under the Monitoring error.
func TestIntegration_MonitoringPermissionDenied(t *testing.T) {
	f := newIntegrationFixture(t)

	const projectID, instanceID = "ip-permdenied", "inst-permdenied"
	const configID = "regional-asia-northeast1"
	f.spannerSrv.AdminUpsertInstance(instanceName(projectID, instanceID), 100, configName(projectID, configID))
	f.spannerSrv.AdminSetQuota(projectID, configID, 100000)
	// Quota store is configured, but quota-mode overrides it.
	f.quotaStore.Set(projectID, "asia-northeast1", monitoringemulator.QuotaEntry{
		QuotaMetric: "spanner.googleapis.com/nodes",
		LimitName:   "SpannerNodesPerProject",
		LimitNodes:  100,
		UsageNodes:  1,
	})
	f.quotaModeStore.Set(codes.PermissionDenied, "permission denied")

	s, labels := f.makeSyncer(t, projectID, instanceID, "permdenied")
	applied, err := s.UpdateInstance(context.Background(), 101000)
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("UpdateInstance err code = %v, want ResourceExhausted; err=%v", status.Code(err), err)
	}
	if applied != 0 {
		t.Errorf("applied = %d, want 0 (no retry)", applied)
	}

	wantIdentity := identityLabels(labels)
	if got := counterValue(t, f.registry, "spanner_autoscaler_quota_lookup_total", merge(wantIdentity, map[string]string{"result": "error", "reason": "permission_denied"})); got != 1 {
		t.Errorf("quota_lookup_total{result=error,reason=permission_denied} = %v, want 1", got)
	}
}

func identityLabels(l observability.Labels) map[string]string {
	return map[string]string{
		"namespace":   l.Namespace,
		"name":        l.Name,
		"project_id":  l.ProjectID,
		"instance_id": l.InstanceID,
	}
}

func merge(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}
