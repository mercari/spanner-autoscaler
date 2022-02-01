package spanner

import (
	"context"
	"sync"
)

// FakeClient implements Client but operates fake objects for testing.
// This ensure thread-safety.
type FakeClient struct {
	instance *Instance

	mu sync.RWMutex
}

// Guarantee *FakeClient implements Client.
var _ Client = (*FakeClient)(nil)

// NewFakeClient returns a new *FakeClient initialized with the given fake objects.
func NewFakeClient(instance *Instance) *FakeClient {
	return &FakeClient{
		instance: instance,
		mu:       sync.RWMutex{},
	}
}

// GetInstance implements Client.
func (c *FakeClient) GetInstance(ctx context.Context) (*Instance, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.instance, nil
}

// UpdateInstance implements Client.
func (c *FakeClient) UpdateInstance(ctx context.Context, instance *Instance) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.instance = instance
	return nil
}
