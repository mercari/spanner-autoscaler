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
	"k8s.io/apimachinery/pkg/util/clock"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlenvtest "sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"

	spannerv1alpha1 "github.com/mercari/spanner-autoscaler/api/v1alpha1"
	"github.com/mercari/spanner-autoscaler/pkg/metrics"
	"github.com/mercari/spanner-autoscaler/pkg/spanner"
	"k8s.io/utils/pointer"
)

var scheme = runtime.NewScheme()

func init() {
	spannerv1alpha1.SchemeBuilder.Register(&spannerv1alpha1.SpannerAutoscaler{}, &spannerv1alpha1.SpannerAutoscalerList{})
	clientgoscheme.AddToScheme(scheme)
	spannerv1alpha1.AddToScheme(scheme)
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
		fakeSpannerAutoscaler = &spannerv1alpha1.SpannerAutoscaler{
			TypeMeta: metav1.TypeMeta{
				Kind:       "SpannerAutoscaler",
				APIVersion: "spanner.mercari.com/v1alpha1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      fakeName,
				Namespace: fakeNamespace,
			},
			Spec: spannerv1alpha1.SpannerAutoscalerSpec{
				ScaleTargetRef: spannerv1alpha1.ScaleTargetRef{
					ProjectID:  pointer.String("fake-project-id"),
					InstanceID: pointer.String(fakeInstanceID),
				},
				ServiceAccountSecretRef: &spannerv1alpha1.ServiceAccountSecretRef{
					Namespace: pointer.String(""),
					Name:      pointer.String("fake-service-account-secret"),
					Key:       pointer.String("fake-service-account-key"),
				},
				MinNodes:          pointer.Int32(1),
				MaxNodes:          pointer.Int32(3),
				MaxScaleDownNodes: pointer.Int32(1),
				TargetCPUUtilization: spannerv1alpha1.TargetCPUUtilization{
					HighPriority: pointer.Int32(50),
				},
			},
			Status: spannerv1alpha1.SpannerAutoscalerStatus{},
		}
	)

	tests := []struct {
		name           string
		fakeInstances  map[string]*spanner.Instance
		fakeMetrics    map[string]*metrics.InstanceMetrics
		targetResource *spannerv1alpha1.SpannerAutoscaler
		want           *spannerv1alpha1.SpannerAutoscaler
		wantErr        bool
	}{
		{
			name: "sync and update instance",
			fakeInstances: map[string]*spanner.Instance{
				fakeInstanceID: {
					ProcessingUnits: pointer.Int32(3000),
					InstanceState:   spanner.StateReady,
				},
			},
			fakeMetrics: map[string]*metrics.InstanceMetrics{
				fakeInstanceID: {
					CurrentHighPriorityCPUUtilization: pointer.Int32(30),
				},
			},
			targetResource: func() *spannerv1alpha1.SpannerAutoscaler {
				o := fakeSpannerAutoscaler.DeepCopy()
				return o
			}(),
			want: func() *spannerv1alpha1.SpannerAutoscaler {
				o := fakeSpannerAutoscaler.DeepCopy()
				o.Status.CurrentNodes = pointer.Int32(3)
				o.Status.CurrentProcessingUnits = pointer.Int32(3000)
				o.Status.InstanceState = spannerv1alpha1.InstanceStateReady
				o.Status.CurrentHighPriorityCPUUtilization = pointer.Int32(30)
				o.Status.LastSyncTime = &metav1.Time{Time: fakeTime}
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
				spannerClient:  spanner.NewFakeClient(tt.fakeInstances),
				metricsClient:  metrics.NewFakeClient(tt.fakeMetrics),
				namespacedName: fakeNamespacedName,
				log: func() logr.Logger {
					l := zap.NewAtomicLevelAt(zap.DebugLevel)
					return ctrlzap.New(ctrlzap.Level(&l))
				}(),
				clock: clock.NewFakeClock(fakeTime),
			}

			if err := s.syncResource(ctx); (err != nil) != tt.wantErr {
				t.Errorf("syncResource() error = %v, wantErr %v", err, tt.wantErr)
			}

			var got spannerv1alpha1.SpannerAutoscaler
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
	fakeInstanceID := "fake-instance-id"

	tests := []struct {
		name                string
		fakeInstances       map[string]*spanner.Instance
		fakeMetrics         map[string]*metrics.InstanceMetrics
		wantInstance        *spanner.Instance
		wantInstanceMetrics *metrics.InstanceMetrics
		wantErr             bool
	}{
		{
			name: "get instance info",
			fakeInstances: map[string]*spanner.Instance{
				fakeInstanceID: {
					ProcessingUnits: pointer.Int32(1000),
					InstanceState:   spanner.StateReady,
				},
			},
			fakeMetrics: map[string]*metrics.InstanceMetrics{
				fakeInstanceID: {
					CurrentHighPriorityCPUUtilization: pointer.Int32(50),
				},
			},
			wantInstance: &spanner.Instance{
				ProcessingUnits: pointer.Int32(1000),
				InstanceState:   spanner.StateReady,
			},
			wantInstanceMetrics: &metrics.InstanceMetrics{
				CurrentHighPriorityCPUUtilization: pointer.Int32(50),
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &syncer{
				spannerClient: spanner.NewFakeClient(tt.fakeInstances),
				metricsClient: metrics.NewFakeClient(tt.fakeMetrics),
				log: func() logr.Logger {
					l := zap.NewAtomicLevelAt(zap.DebugLevel)
					return ctrlzap.New(ctrlzap.Level(&l))
				}(),
			}

			ctx := context.Background()
			gotInstance, gotInstanceMetrics, err := s.getInstanceInfo(ctx, fakeInstanceID)
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
