package spanner

import (
	"context"
	"fmt"
	"sync"
)

// FakeClient implements Client but operates fake objects for testing.
// This ensure thread-safety.
type FakeClient struct {
	instances map[string]*Instance

	mu sync.RWMutex
}

// Guarantee *FakeClient implements Client.
var _ Client = (*FakeClient)(nil)

// NewFakeClient returns a new *FakeClient initialized with the given fake objects.
func NewFakeClient(instances map[string]*Instance) *FakeClient {
	return &FakeClient{
		instances: instances,
		mu:        sync.RWMutex{},
	}
}

// GetInstance implements Client.
func (c *FakeClient) GetInstance(ctx context.Context, instanceID string) (*Instance, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	instance, ok := c.instances[instanceID]
	if !ok {
		return nil, fmt.Errorf("instance %q not found", instanceID)
	}
	return instance, nil
}

// UpdateInstance implements Client.
func (c *FakeClient) UpdateInstance(ctx context.Context, instanceID string, instance *Instance) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.instances[instanceID]; !ok {
		return fmt.Errorf("instance %q not found", instanceID)
	}
	c.instances[instanceID] = instance
	return nil
}
