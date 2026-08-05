package spanner

import (
	"context"
	"fmt"

	spanneradmin "cloud.google.com/go/spanner/admin/instance/apiv1"
	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"github.com/go-logr/logr"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	field_mask "google.golang.org/genproto/protobuf/field_mask"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

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
	Config          string
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

	endpoint    string
	tokenSource oauth2.TokenSource
	log         logr.Logger
}

var _ Client = (*client)(nil)

type Option func(*client)

func WithEndpoint(endpoint string) Option {
	return func(c *client) {
		c.endpoint = endpoint
	}
}

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

	if c.endpoint != "" {
		options = append(options,
			option.WithEndpoint(c.endpoint),
			option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
		)
	} else if c.tokenSource != nil {
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
		Config:          i.GetConfig(),
	}, nil
}

// UpdateInstance implements Client.
//
// Only processing_units is mutated, so the request carries a minimal Instance
// (Name + ProcessingUnits) with FieldMask=["processing_units"]. There is no
// pre-flight GetInstance: the field mask makes a full instance round-trip
// unnecessary, and on ResourceExhausted the syncer fetches the instance once
// for fallback math instead of paying for it on every UpdateInstance attempt.
func (c *client) UpdateInstance(ctx context.Context, instance *Instance) error {
	log := c.log.WithValues("instance-id", c.instanceID, "project-id", c.projectID)

	name := fmt.Sprintf(instanceNameFormat, c.projectID, c.instanceID)
	log.V(1).Info("updating spanner instance", "patch", instance)

	_, err := c.spannerInstanceAdminClient.UpdateInstance(ctx, &instancepb.UpdateInstanceRequest{
		Instance: &instancepb.Instance{
			Name: name,
			//nolint:gosec // G115
			ProcessingUnits: int32(instance.ProcessingUnits),
		},
		FieldMask: &field_mask.FieldMask{
			Paths: []string{"processing_units"},
		},
	})
	if err != nil {
		log.Error(err, "unable to update spanner instance")
		return err
	}
	log.V(1).Info("updated spanner instance", "applied patch", instance)

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
