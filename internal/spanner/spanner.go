package spanner

import (
	"context"
	"fmt"

	spanneradmin "cloud.google.com/go/spanner/admin/instance/apiv1"
	"github.com/go-logr/logr"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	instancepb "google.golang.org/genproto/googleapis/spanner/admin/instance/v1"
	field_mask "google.golang.org/genproto/protobuf/field_mask"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
)

const instanceNameFormat = "projects/%s/instances/%s"

type State = spannerv1beta1.InstanceState

const (
	StateUnspecified = spannerv1beta1.InstanceStateUnspecified
	StateCreating    = spannerv1beta1.InstanceStateCreating
	StateReady       = spannerv1beta1.InstanceStateReady
)

// Instance represents Spanner Instance.
type Instance struct {
	ProcessingUnits int
	InstanceState   State
}

// Client is a client for manipulation of Instance.
type Client interface {
	// GetInstance gets the instance by instance id.
	GetInstance(ctx context.Context) (*Instance, error)
	// GetInstanceMetrics updates the instance whose provided instance id.
	UpdateInstance(ctx context.Context, instance *Instance) error
}

type client struct {
	spannerInstanceAdminClient *spanneradmin.InstanceAdminClient

	projectID  string
	instanceID string

	tokenSource oauth2.TokenSource
	log         logr.Logger
}

var _ Client = (*client)(nil)

type Option func(*client)

func WithTokenSource(ts oauth2.TokenSource) Option {
	return func(c *client) {
		c.tokenSource = ts
	}
}

func WithLog(log logr.Logger) Option {
	return func(c *client) {
		c.log = log.WithName("spanner")
	}
}

// NewClient returns a new Client.
func NewClient(ctx context.Context, projectID, instanceID string, opts ...Option) (Client, error) {
	c := &client{
		projectID:  projectID,
		instanceID: instanceID,
		log:        logr.Discard(),
	}

	for _, opt := range opts {
		opt(c)
	}

	var options []option.ClientOption

	if c.tokenSource != nil {
		options = append(options, option.WithTokenSource(c.tokenSource))
	}

	spannerInstanceAdminClient, err := spanneradmin.NewInstanceAdminClient(ctx, options...)
	if err != nil {
		return nil, err
	}

	c.spannerInstanceAdminClient = spannerInstanceAdminClient

	return c, nil
}

// GetInstance implements Client.
func (c *client) GetInstance(ctx context.Context) (*Instance, error) {
	log := c.log.WithValues("instance-id", c.instanceID, "project-id", c.projectID)

	log.V(1).Info("getting spanner instance")

	i, err := c.spannerInstanceAdminClient.GetInstance(ctx, &instancepb.GetInstanceRequest{
		Name: fmt.Sprintf(instanceNameFormat, c.projectID, c.instanceID),
	})
	if err != nil {
		log.Error(err, "unable to get spanner instance")
		return nil, err
	}

	log.V(1).Info("got spanner instance", "received instance", i)
	return &Instance{
		ProcessingUnits: int(i.ProcessingUnits),
		InstanceState:   instanceState(i.State),
	}, nil
}

// UpdateInstance implements Client.
func (c *client) UpdateInstance(ctx context.Context, instance *Instance) error {
	log := c.log.WithValues("instance-id", c.instanceID, "project-id", c.projectID)

	log.V(1).Info("getting spanner instance")
	i, err := c.spannerInstanceAdminClient.GetInstance(ctx, &instancepb.GetInstanceRequest{
		Name: fmt.Sprintf(instanceNameFormat, c.projectID, c.instanceID),
	})
	if err != nil {
		log.Error(err, "unable to get spanner instance")
		return err
	}

	log.V(1).Info("got spanner instance", "received instance", i, "patch", instance)

	i.ProcessingUnits = int32(instance.ProcessingUnits)
	log.V(1).Info("updating spanner instance", "desired instance", i, "patch", instance)

	_, err = c.spannerInstanceAdminClient.UpdateInstance(ctx, &instancepb.UpdateInstanceRequest{
		Instance: i,
		FieldMask: &field_mask.FieldMask{
			Paths: []string{"processing_units"},
		},
	})
	if err != nil {
		log.Error(err, "unable to update spanner instance")
		return err
	}
	log.V(1).Info("updated spanner instance", "desired instance", i, "applied patch", instance)

	return nil
}

func instanceState(s instancepb.Instance_State) State {
	switch s {
	case instancepb.Instance_STATE_UNSPECIFIED:
		return StateUnspecified
	case instancepb.Instance_CREATING:
		return StateCreating
	case instancepb.Instance_READY:
		return StateReady
	default:
		return StateUnspecified
	}
}
