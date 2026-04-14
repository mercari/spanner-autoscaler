package fake

import (
	"context"
	"sync"

	"github.com/mercari/spanner-autoscaler/internal/spanner"
)

// Client is an in-memory fake spanner.Client for use in integration tests.
// The Cloud Spanner Emulator does not support UpdateInstance, so this client
// simulates instance state locally.
type Client struct {
	mu              sync.Mutex
	processingUnits int
	instanceState   spanner.State
}

var _ spanner.Client = (*Client)(nil)

func NewClient(processingUnits int) *Client {
	return &Client{
		processingUnits: processingUnits,
		instanceState:   spanner.StateReady,
	}
}

func (c *Client) GetInstance(_ context.Context) (*spanner.Instance, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &spanner.Instance{
		ProcessingUnits: c.processingUnits,
		InstanceState:   c.instanceState,
	}, nil
}

func (c *Client) UpdateInstance(_ context.Context, instance *spanner.Instance) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.processingUnits = instance.ProcessingUnits
	return nil
}
