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
func (c *FakeClient) GetInstanceMetrics(ctx context.Context) (*InstanceMetrics, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.metrics, nil
}
