package metrics

import (
	"context"
	"fmt"
	"sync"
)

// FakeClient implements Client but operates fake objects for testing.
// This ensure thread-safety.
type FakeClient struct {
	metrics map[string]*InstanceMetrics

	mu sync.RWMutex
}

// Guarantee *FakeClient implements Client.
var _ Client = (*FakeClient)(nil)

// NewFakeClient returns a new *FakeClient initialized with the given fake objects.
func NewFakeClient(metrics map[string]*InstanceMetrics) *FakeClient {
	return &FakeClient{
		metrics: metrics,
		mu:      sync.RWMutex{},
	}
}

// GetInstanceMetrics implements Client.
func (c *FakeClient) GetInstanceMetrics(ctx context.Context, instanceID string) (*InstanceMetrics, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.metrics[instanceID]
	if !ok {
		return nil, fmt.Errorf("metrics %q not found", instanceID)
	}
	return m, nil
}
