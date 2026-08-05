package metrics

import (
	"context"
	"sync"
	"time"
)

// FakeClient implements Client but operates fake objects for testing.
// This ensure thread-safety.
type FakeClient struct {
	metrics  *InstanceMetrics
	quota    *Quota
	quotaErr error

	mu sync.RWMutex
}

// Guarantee *FakeClient implements Client.
var _ Client = (*FakeClient)(nil)

// NewFakeClient returns a new *FakeClient initialized with the given fake objects.
func NewFakeClient(metrics *InstanceMetrics) *FakeClient {
	return &FakeClient{
		metrics: metrics,
		mu:      sync.RWMutex{},
	}
}

func (c *FakeClient) SetQuota(quota *Quota) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.quota = quota
	c.quotaErr = nil
}

func (c *FakeClient) SetQuotaError(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.quotaErr = err
}

// GetInstanceMetrics implements Client.
// It returns a copy of the stored metrics with only the field corresponding to
// metricType populated, mirroring the real client's behavior and preventing the
// other field from masking bugs caused by stale values. The now argument is
// accepted to satisfy the Client interface and is otherwise ignored.
func (c *FakeClient) GetInstanceMetrics(ctx context.Context, metricType MetricType, _ time.Time) (*InstanceMetrics, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	switch metricType {
	case MetricTypeTotal:
		return &InstanceMetrics{CurrentTotalCPUUtilization: c.metrics.CurrentTotalCPUUtilization}, nil
	default: // MetricTypeHighPriority
		return &InstanceMetrics{CurrentHighPriorityCPUUtilization: c.metrics.CurrentHighPriorityCPUUtilization}, nil
	}
}

// GetQuota implements Client.
func (c *FakeClient) GetQuota(_ context.Context, _ string, _ time.Time) (*Quota, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.quotaErr != nil {
		return nil, c.quotaErr
	}
	if c.quota == nil {
		return nil, ErrQuotaNoData
	}
	return &Quota{
		LimitNodes:  c.quota.LimitNodes,
		UsageNodes:  c.quota.UsageNodes,
		QuotaMetric: c.quota.QuotaMetric,
		LimitName:   c.quota.LimitName,
		Location:    c.quota.Location,
	}, nil
}
