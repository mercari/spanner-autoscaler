package syncer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	testingclock "k8s.io/utils/clock/testing"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlenvtest "sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	"github.com/mercari/spanner-autoscaler/internal/metrics"
	"github.com/mercari/spanner-autoscaler/internal/spanner"
)

var scheme = runtime.NewScheme()

func init() {
	spannerv1beta1.SchemeBuilder.Register(&spannerv1beta1.SpannerAutoscaler{}, &spannerv1beta1.SpannerAutoscalerList{})
	clientgoscheme.AddToScheme(scheme)
	spannerv1beta1.AddToScheme(scheme)
	apiextensionsv1.AddToScheme(scheme)
}

func Test_syncer_syncResource(t *testing.T) {
	var (
		fakeName           = "fake-spanner-autoscaler"
		fakeNamespace      = "fake-namespace"
		fakeNamespacedName = types.NamespacedName{
			Namespace: fakeNamespace,
			Name:      fakeName,
		}
		fakeInstanceID        = "fake-instance-id"
		fakeTime              = time.Date(2020, 4, 1, 0, 0, 0, 0, time.Local)
		fakeSpannerAutoscaler = &spannerv1beta1.SpannerAutoscaler{
			TypeMeta: metav1.TypeMeta{
				Kind:       "SpannerAutoscaler",
				APIVersion: "spanner.mercari.com/v1beta1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      fakeName,
				Namespace: fakeNamespace,
			},
			Spec: spannerv1beta1.SpannerAutoscalerSpec{
				TargetInstance: spannerv1beta1.TargetInstance{
					ProjectID:  "fake-project-id",
					InstanceID: fakeInstanceID,
				},
				Authentication: spannerv1beta1.Authentication{
					Type: spannerv1beta1.AuthTypeSA,
					IAMKeySecret: &spannerv1beta1.IAMKeySecret{
						Namespace: "",
						Name:      "fake-service-account-secret",
						Key:       "fake-service-account-key",
					},
				},
				ScaleConfig: spannerv1beta1.ScaleConfig{
					ComputeType: spannerv1beta1.ComputeTypeNode,
					Nodes: spannerv1beta1.ScaleConfigNodes{
						Min: 1,
						Max: 3,
					},
					ScaledownStepSize: 1000,
					TargetCPUUtilization: spannerv1beta1.TargetCPUUtilization{
						HighPriority: 50,
					},
				},
			},
			Status: spannerv1beta1.SpannerAutoscalerStatus{},
		}
	)

	tests := []struct {
		name           string
		fakeInstance   *spanner.Instance
		fakeMetrics    *metrics.InstanceMetrics
		targetResource *spannerv1beta1.SpannerAutoscaler
		want           *spannerv1beta1.SpannerAutoscaler
		wantErr        bool
	}{
		{
			name: "sync and update instance",
			fakeInstance: &spanner.Instance{
				ProcessingUnits: 3000,
				InstanceState:   spanner.StateReady,
			},
			fakeMetrics: &metrics.InstanceMetrics{
				CurrentHighPriorityCPUUtilization: 30,
			},
			targetResource: func() *spannerv1beta1.SpannerAutoscaler {
				o := fakeSpannerAutoscaler.DeepCopy()
				return o
			}(),
			want: func() *spannerv1beta1.SpannerAutoscaler {
				o := fakeSpannerAutoscaler.DeepCopy()
				o.Status.CurrentProcessingUnits = 3000
				o.Status.InstanceState = spannerv1beta1.InstanceStateReady
				o.Status.CurrentHighPriorityCPUUtilization = 30
				o.Status.LastSyncTime = metav1.Time{Time: fakeTime}
				return o
			}(),
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			te := ctrlenvtest.Environment{
				CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
			}
			cfg, err := te.Start()
			if err != nil {
				t.Fatalf("unable to start test environment: %v", err)
			}
			defer te.Stop()

			ctrlClient, err := ctrlclient.New(cfg, ctrlclient.Options{
				Scheme: scheme,
			})
			if err != nil {
				t.Fatalf("unable to new controller client: %v", err)
			}

			ctx := context.Background()

			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: fakeNamespace,
				},
			}

			if err := ctrlClient.Create(ctx, ns); err != nil {
				t.Fatalf("failed to create namespace: %s", err)
			}

			if err := ctrlClient.Create(ctx, tt.targetResource); err != nil {
				t.Fatalf("unable to create SpannerAutoscaler: %v", err)
			}
			// Update object status via ctrlclient.StatusWriter because ctrlclient.Create does not create an object including status.
			if err := ctrlClient.Status().Update(ctx, tt.targetResource); err != nil {
				t.Fatalf("unable to update SpannerAutoscaler status: %v", err)
			}

			s := &syncer{
				ctrlClient:     ctrlClient,
				spannerClient:  spanner.NewFakeClient(tt.fakeInstance),
				metricsClient:  metrics.NewFakeClient(tt.fakeMetrics),
				namespacedName: fakeNamespacedName,
				log: func() logr.Logger {
					l := zap.NewAtomicLevelAt(zap.DebugLevel)
					return ctrlzap.New(ctrlzap.Level(&l))
				}(),
				clock: testingclock.NewFakeClock(fakeTime),
			}

			if err := s.syncResource(ctx); (err != nil) != tt.wantErr {
				t.Errorf("syncResource() error = %v, wantErr %v", err, tt.wantErr)
			}

			var got spannerv1beta1.SpannerAutoscaler
			err = ctrlClient.Get(ctx, fakeNamespacedName, &got)
			if err != nil {
				t.Fatalf("unable to get SpannerAutoscaler resource: %v", err)
			}

			if diff := cmp.Diff(tt.want.Status, got.Status); diff != "" {
				t.Fatalf("(-wantInstance, +got)\n%s", diff)
			}
		})
	}
}

func Test_syncer_getInstanceInfo(t *testing.T) {
	tests := []struct {
		name                string
		fakeInstance        *spanner.Instance
		fakeMetrics         *metrics.InstanceMetrics
		wantInstance        *spanner.Instance
		wantInstanceMetrics *metrics.InstanceMetrics
		wantErr             bool
	}{
		{
			name: "get instance info",
			fakeInstance: &spanner.Instance{
				ProcessingUnits: 1000,
				InstanceState:   spanner.StateReady,
			},
			fakeMetrics: &metrics.InstanceMetrics{
				CurrentHighPriorityCPUUtilization: 50,
			},
			wantInstance: &spanner.Instance{
				ProcessingUnits: 1000,
				InstanceState:   spanner.StateReady,
			},
			wantInstanceMetrics: &metrics.InstanceMetrics{
				CurrentHighPriorityCPUUtilization: 50,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &syncer{
				spannerClient: spanner.NewFakeClient(tt.fakeInstance),
				metricsClient: metrics.NewFakeClient(tt.fakeMetrics),
				log: func() logr.Logger {
					l := zap.NewAtomicLevelAt(zap.DebugLevel)
					return ctrlzap.New(ctrlzap.Level(&l))
				}(),
			}

			ctx := context.Background()
			gotInstance, gotInstanceMetrics, err := s.getInstanceInfo(ctx)
			if (err != nil) != tt.wantErr {
				t.Errorf("getInstanceInfo() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if diff := cmp.Diff(gotInstance, tt.wantInstance); diff != "" {
				t.Errorf("getInstanceInfo() gotInstance = %v, wantInstance %v", gotInstance, tt.wantInstance)
			}
			if diff := cmp.Diff(gotInstanceMetrics, tt.wantInstanceMetrics); diff != "" {
				t.Errorf("getInstanceInfo() gotInstanceMetrics = %v, wantInstance %v", gotInstanceMetrics, tt.wantInstanceMetrics)
			}
		})
	}
}
