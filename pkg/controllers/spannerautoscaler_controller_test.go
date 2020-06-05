package controllers

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/clock"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	spannerv1alpha1 "github.com/mercari/spanner-autoscaler/pkg/api/v1alpha1"
	"github.com/mercari/spanner-autoscaler/pkg/pointer"
	"github.com/mercari/spanner-autoscaler/pkg/syncer"
	fakesyncer "github.com/mercari/spanner-autoscaler/pkg/syncer/fake"
)

var scheme = runtime.NewScheme()

func init() {
	spannerv1alpha1.SchemeBuilder.Register(&spannerv1alpha1.SpannerAutoscaler{}, &spannerv1alpha1.SpannerAutoscalerList{})
	//nolint:errcheck
	clientgoscheme.AddToScheme(scheme)
	//nolint:errcheck
	spannerv1alpha1.AddToScheme(scheme)
	//nolint:errcheck
	apiextensionsv1.AddToScheme(scheme)
}

func TestSpannerAutoscalerReconciler_Reconcile(t *testing.T) {
	cli := setupEnvtest(t)

	fakeTime := time.Date(2020, 4, 1, 0, 0, 0, 0, time.Local)
	name := "test-spanner-autoscaler"
	namespace := "test-namespace"

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	}

	ctx := context.Background()

	if err := cli.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %s", err)
	}

	namespacedName := types.NamespacedName{
		Namespace: namespace,
		Name:      name,
	}

	baseObj := &spannerv1alpha1.SpannerAutoscaler{
		TypeMeta: metav1.TypeMeta{
			Kind:       "SpannerAutoscaler",
			APIVersion: "spanner.mercari.com/v1alpha1",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: spannerv1alpha1.SpannerAutoscalerSpec{
			ScaleTargetRef: spannerv1alpha1.ScaleTargetRef{
				ProjectID:  pointer.String("test-project-id"),
				InstanceID: pointer.String("test-instance-id"),
			},
			ServiceAccountSecretRef: spannerv1alpha1.ServiceAccountSecretRef{
				Name:      pointer.String("test-service-account-secret"),
				Namespace: pointer.String(""),
				Key:       pointer.String("secret"),
			},
		},
		Status: spannerv1alpha1.SpannerAutoscalerStatus{},
	}

	type fields struct {
		syncers map[types.NamespacedName]syncer.Syncer
	}

	tests := []struct {
		name           string
		fields         fields
		secret         *corev1.Secret
		targetResource *spannerv1alpha1.SpannerAutoscaler
		want           *spannerv1alpha1.SpannerAutoscaler
		wantErr        bool
	}{
		{
			name: "scale up spanner instance nodes",
			fields: fields{
				syncers: map[types.NamespacedName]syncer.Syncer{
					namespacedName: &fakesyncer.Syncer{},
				},
			},
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-account-secret",
					Namespace: namespace,
				},
				StringData: map[string]string{"secret": `{"foo":"bar"}`},
			},
			targetResource: func() *spannerv1alpha1.SpannerAutoscaler {
				o := baseObj.DeepCopy()
				o.Spec.MinNodes = pointer.Int32(1)
				o.Spec.MaxNodes = pointer.Int32(10)
				o.Spec.MaxScaleDownNodes = pointer.Int32(2)
				o.Spec.TargetCPUUtilization = spannerv1alpha1.TargetCPUUtilization{
					HighPriority: pointer.Int32(30),
				}
				o.Status.CurrentNodes = pointer.Int32(3)
				o.Status.InstanceState = spannerv1alpha1.InstanceStateReady
				o.Status.LastScaleTime = &metav1.Time{Time: fakeTime.Add(-2 * time.Hour)} // more than scaleDownInterval
				o.Status.CurrentHighPriorityCPUUtilization = pointer.Int32(50)
				return o
			}(),
			want: func() *spannerv1alpha1.SpannerAutoscaler {
				o := baseObj.DeepCopy()
				o.Status.DesiredNodes = pointer.Int32(6)
				o.Status.InstanceState = spannerv1alpha1.InstanceStateReady
				o.Status.LastScaleTime = &metav1.Time{Time: fakeTime}
				return o
			}(),
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.secret != nil {
				err := cli.Create(ctx, tt.secret)
				if err != nil {
					t.Fatalf("failed to create secret: %+v", err)
				}
			}

			if err := cli.Create(ctx, tt.targetResource); err != nil {
				t.Fatalf("unable to create SpannerAutoscaler: %v", err)
			}
			// Update object status via ctrlclient.StatusWriter because ctrlclient.Create does not create an object including status.
			if err := cli.Status().Update(ctx, tt.targetResource); err != nil {
				t.Fatalf("unable to update SpannerAutoscaler status: %v", err)
			}

			r := NewSpannerAutoscalerReconciler(
				cli,
				cli,
				scheme,
				record.NewFakeRecorder(10),
				WithSyncers(tt.fields.syncers),
				WithLog(func() logr.Logger {
					l := zap.NewAtomicLevelAt(zap.DebugLevel)
					return ctrlzap.New(ctrlzap.Level(&l))
				}()),
				WithScaleDownInterval(time.Hour),
				WithClock(clock.NewFakeClock(fakeTime)),
			)

			res, err := r.Reconcile(ctrlreconcile.Request{
				NamespacedName: namespacedName,
			})
			if (err != nil) != tt.wantErr {
				t.Fatalf("Reconcile() error = %v, wantErr %v", err, tt.wantErr)
			}
			if res.Requeue {
				t.Fatalf("result.Requeue is true: %t", res.Requeue)
			}

			var got spannerv1alpha1.SpannerAutoscaler
			err = cli.Get(ctx, namespacedName, &got)
			if err != nil {
				t.Fatalf("unable to get SpannerAutoscaler resource: %v", err)
			}

			if diff := cmp.Diff(tt.want.Status, got.Status,
				// Ignore CurrentNodes because syncer.Syncer updates it.
				cmpopts.IgnoreFields(tt.want.Status, "CurrentNodes"),
				// Ignore CurrentHighPriorityCPUUtilization because controller doesn't update it.
				cmpopts.IgnoreFields(tt.want.Status, "CurrentHighPriorityCPUUtilization"),
			); diff != "" {
				t.Fatalf("(-want, +got)\n%s", diff)
			}
		})
	}
}

func TestSpannerAutoscalerReconciler_needCalcNodes(t *testing.T) {
	fakeTime := time.Date(2020, 4, 1, 0, 0, 0, 0, time.Local)

	type args struct {
		sa *spannerv1alpha1.SpannerAutoscaler
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "need",
			args: args{
				sa: &spannerv1alpha1.SpannerAutoscaler{
					Status: spannerv1alpha1.SpannerAutoscalerStatus{
						LastScaleTime: &metav1.Time{Time: fakeTime.Add(-2 * time.Hour)},
						CurrentNodes:  pointer.Int32(1),
						InstanceState: spannerv1alpha1.InstanceStateReady,
					},
				},
			},
			want: true,
		},
		{
			name: "no need because current nodes have not fetched yet",
			args: args{
				sa: &spannerv1alpha1.SpannerAutoscaler{},
			},
			want: false,
		},
		{
			name: "no need because instance state is not ready",
			args: args{
				sa: &spannerv1alpha1.SpannerAutoscaler{
					Status: spannerv1alpha1.SpannerAutoscalerStatus{
						CurrentNodes:  pointer.Int32(1),
						InstanceState: spannerv1alpha1.InstanceStateCreating,
					},
				},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &SpannerAutoscalerReconciler{
				scaleDownInterval: time.Hour,
				clock:             clock.NewFakeClock(fakeTime),
				log:               zapr.NewLogger(zap.NewNop()),
			}
			got := r.needCalcNodes(tt.args.sa)
			if got != tt.want {
				t.Errorf("needCalcNodes() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSpannerAutoscalerReconciler_needUpdateNodes(t *testing.T) {
	fakeTime := time.Date(2020, 4, 1, 0, 0, 0, 0, time.Local)

	type args struct {
		sa *spannerv1alpha1.SpannerAutoscaler
	}
	tests := []struct {
		name string
		args args
		want bool
	}{
		{
			name: "no need because not so long to scale down since instance scaled nodes last",
			args: args{
				sa: &spannerv1alpha1.SpannerAutoscaler{
					Status: spannerv1alpha1.SpannerAutoscalerStatus{
						LastScaleTime: &metav1.Time{Time: fakeTime.Add(-time.Minute)},
						CurrentNodes:  pointer.Int32(2),
						DesiredNodes:  pointer.Int32(1),
						InstanceState: spannerv1alpha1.InstanceStateReady,
					},
				},
			},
			want: false,
		},
		{
			name: "need because interval is not applied when scaling down",
			args: args{
				sa: &spannerv1alpha1.SpannerAutoscaler{
					Status: spannerv1alpha1.SpannerAutoscalerStatus{
						LastScaleTime: &metav1.Time{Time: fakeTime.Add(-time.Minute)},
						CurrentNodes:  pointer.Int32(1),
						DesiredNodes:  pointer.Int32(2),
						InstanceState: spannerv1alpha1.InstanceStateReady,
					},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &SpannerAutoscalerReconciler{
				scaleDownInterval: time.Hour,
				clock:             clock.NewFakeClock(fakeTime),
				log:               zapr.NewLogger(zap.NewNop()),
			}
			got := r.needUpdateNodes(tt.args.sa, *tt.args.sa.Status.DesiredNodes)
			if got != tt.want {
				t.Errorf("needUpdateNodes() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func Test_calcDesiredNodes(t *testing.T) {
	type args struct {
		currentCPU        int32
		currentNodes      int32
		targetCPU         int32
		minNodes          int32
		maxNodes          int32
		maxScaleDownNodes int32
	}
	tests := []struct {
		name string
		args args
		want int32
	}{
		{
			name: "no scale",
			args: args{
				currentCPU:        25,
				currentNodes:      2,
				targetCPU:         30,
				minNodes:          1,
				maxNodes:          10,
				maxScaleDownNodes: 2,
			},
			want: 2,
		},
		{
			name: "scale up",
			args: args{
				currentCPU:        50,
				currentNodes:      3,
				targetCPU:         30,
				minNodes:          1,
				maxNodes:          10,
				maxScaleDownNodes: 2,
			},
			want: 6,
		},
		{
			name: "scale down",
			args: args{
				currentCPU:        30,
				currentNodes:      5,
				targetCPU:         50,
				minNodes:          1,
				maxNodes:          10,
				maxScaleDownNodes: 2,
			},
			want: 4,
		},
		{
			name: "scale up to max nodes",
			args: args{
				currentCPU:        50,
				currentNodes:      3,
				targetCPU:         30,
				minNodes:          1,
				maxNodes:          4,
				maxScaleDownNodes: 2,
			},
			want: 4,
		},
		{
			name: "scale down to min nodes",
			args: args{
				currentCPU:        30,
				currentNodes:      5,
				targetCPU:         50,
				minNodes:          5,
				maxNodes:          10,
				maxScaleDownNodes: 2,
			},
			want: 5,
		},
		{
			name: "scale down with max scale down nodes",
			args: args{
				currentCPU:        30,
				currentNodes:      10,
				targetCPU:         50,
				minNodes:          5,
				maxNodes:          10,
				maxScaleDownNodes: 2,
			},
			want: 8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := calcDesiredNodes(tt.args.currentCPU, tt.args.currentNodes, tt.args.targetCPU, tt.args.minNodes, tt.args.maxNodes, tt.args.maxScaleDownNodes); got != tt.want {
				t.Errorf("calcDesiredNodes() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSpannerAutoscalerReconciler_fetchServiceAccountJSON(t *testing.T) {
	ctx := context.Background()

	cli := setupEnvtest(t)

	namespace := "test-namespace"
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	}

	if err := cli.Create(ctx, ns); err != nil {
		t.Fatalf("failed to create namespace: %s", err)
	}

	tests := []struct {
		name        string
		secret      *corev1.Secret
		resource    spannerv1alpha1.SpannerAutoscaler
		expected    []byte
		expectedErr error
	}{
		{
			name: "fetch json correctly",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "secret-1",
					Namespace: namespace,
				},
				StringData: map[string]string{"service-account": `{"foo":"bar"}`},
			},
			resource: spannerv1alpha1.SpannerAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "spanner-autoscaler",
					Namespace: namespace,
				},
				Spec: spannerv1alpha1.SpannerAutoscalerSpec{
					ServiceAccountSecretRef: spannerv1alpha1.ServiceAccountSecretRef{
						Namespace: pointer.String(namespace),
						Name:      pointer.String("secret-1"),
						Key:       pointer.String("service-account"),
					},
				},
			},
			expected:    []byte(`{"foo":"bar"}`),
			expectedErr: nil,
		},
		{
			name: "fetch json correctly even though ServiceAccountSecretRef does not have namespace",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "secret-2",
					Namespace: namespace,
				},
				StringData: map[string]string{"service-account": `{"foo":"bar"}`},
			},
			resource: spannerv1alpha1.SpannerAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "spanner-autoscaler",
					Namespace: namespace,
				},
				Spec: spannerv1alpha1.SpannerAutoscalerSpec{
					ServiceAccountSecretRef: spannerv1alpha1.ServiceAccountSecretRef{
						Name: pointer.String("secret-2"),
						Key:  pointer.String("service-account"),
					},
				},
			},
			expected:    []byte(`{"foo":"bar"}`),
			expectedErr: nil,
		},
		{
			name: "return error when no Secret data found by ServiceAccountSecretRef.Key",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "secret-3",
					Namespace: namespace,
				},
				StringData: map[string]string{"service-account": `{"foo":"bar"}`},
			},
			resource: spannerv1alpha1.SpannerAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "spanner-autoscaler",
					Namespace: namespace,
				},
				Spec: spannerv1alpha1.SpannerAutoscalerSpec{
					ServiceAccountSecretRef: spannerv1alpha1.ServiceAccountSecretRef{
						Namespace: pointer.String(namespace),
						Name:      pointer.String("secret-3"),
						Key:       pointer.String("invalid-key"),
					},
				},
			},
			expected:    nil,
			expectedErr: errFetchServiceAccountJSONNoSecretDataFound,
		},
		{
			name: "return error when no secret name specified",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "secret-4",
					Namespace: namespace,
				},
				StringData: map[string]string{"service-account": `{"foo":"bar"}`},
			},
			resource: spannerv1alpha1.SpannerAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "spanner-autoscaler",
					Namespace: namespace,
				},
				Spec: spannerv1alpha1.SpannerAutoscalerSpec{
					ServiceAccountSecretRef: spannerv1alpha1.ServiceAccountSecretRef{
						Namespace: pointer.String(namespace),
						Key:       pointer.String("service-account"),
					},
				},
			},
			expected:    nil,
			expectedErr: errFetchServiceAccountJSONNoNameSpecified,
		},
		{
			name: "return error when no secret key specified",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "secret-5",
					Namespace: namespace,
				},
				StringData: map[string]string{"service-account": `{"foo":"bar"}`},
			},
			resource: spannerv1alpha1.SpannerAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "spanner-autoscaler",
					Namespace: namespace,
				},
				Spec: spannerv1alpha1.SpannerAutoscalerSpec{
					ServiceAccountSecretRef: spannerv1alpha1.ServiceAccountSecretRef{
						Namespace: pointer.String(namespace),
						Name:      pointer.String("secret-5"),
					},
				},
			},
			expected:    nil,
			expectedErr: errFetchServiceAccountJSONNoKeySpecified,
		},
		{
			name:   "return error when no Secret found by ServiceAccountSecretRef",
			secret: nil,
			resource: spannerv1alpha1.SpannerAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "spanner-autoscaler",
					Namespace: namespace,
				},
				Spec: spannerv1alpha1.SpannerAutoscalerSpec{
					ServiceAccountSecretRef: spannerv1alpha1.ServiceAccountSecretRef{
						Name: pointer.String("no-secret-found"),
						Key:  pointer.String("service-account"),
					},
				},
			},
			expected:    nil,
			expectedErr: errFetchServiceAccountJSONNoSecretFound,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			if tt.secret != nil {
				err := cli.Create(ctx, tt.secret)
				if err != nil {
					t.Fatalf("failed to create secret: %+v", err)
				}
			}

			reconciler := &SpannerAutoscalerReconciler{
				ctrlClient: cli,
				apiReader:  cli,
			}

			saj, err := reconciler.fetchServiceAccountJSON(ctx, &tt.resource)

			if tt.expectedErr == nil && err != nil {
				t.Fatalf("caught an unexpected error: %s", err)
			}

			if tt.expectedErr != nil && !errors.Is(err, tt.expectedErr) {
				t.Errorf("want error: `%s`, but got `%s`", tt.expectedErr, err)
			}

			if want, got := string(tt.expected), string(saj); want != got {
				t.Errorf("want: `%s`, but got `%s`", want, got)
			}
		})
	}
}

func setupEnvtest(t *testing.T) client.Client {
	t.Helper()

	testEnv := envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("faileld to start test env: %s", err)
	}

	c, err := client.New(cfg, client.Options{
		Scheme: scheme,
	})
	if err != nil {
		t.Errorf("faileld to create controller-runtime client: %s", err)

		if err := testEnv.Stop(); err != nil {
			t.Errorf("failed to stop test env: %s", err)
		}

		t.FailNow()
	}

	t.Cleanup(func() {
		if err := testEnv.Stop(); err != nil {
			t.Fatalf("failed to stop test env: %s", err)
		}
	})

	return c
}
