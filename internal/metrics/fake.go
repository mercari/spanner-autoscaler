package metrics

import (
	"context"
	"sync"
)

// FakeClient implements Client but operates fake objects for testing.
// This ensure thread-safety.
type FakeClient struct {
	metrics *InstanceMetrics

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

// GetInstanceMetrics implements Client.
// It returns a copy of the stored metrics with only the field corresponding to
// metricType populated, mirroring the real client's behavior and preventing the
// other field from masking bugs caused by stale values.
func (c *FakeClient) GetInstanceMetrics(ctx context.Context, metricType MetricType) (*InstanceMetrics, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	switch metricType {
	case MetricTypeTotal:
		return &InstanceMetrics{CurrentTotalCPUUtilization: c.metrics.CurrentTotalCPUUtilization}, nil
	default: // MetricTypeHighPriority
		return &InstanceMetrics{CurrentHighPriorityCPUUtilization: c.metrics.CurrentHighPriorityCPUUtilization}, nil
	}
}
