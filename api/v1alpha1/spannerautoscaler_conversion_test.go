package v1alpha1

import (
	"github.com/mercari/spanner-autoscaler/api/v1beta1"
	"github.com/mercari/spanner-autoscaler/utils/pointer"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"testing"
	"time"
)

func TestSpannerAutoscaler_ConvertTo(t *testing.T) {
	t.Parallel()
	timestamp := time.Now()

	tests := []struct {
		title    string
		src      *SpannerAutoscaler
		expected *v1beta1.SpannerAutoscaler
	}{
		{
			title: "target instance",
			src: &SpannerAutoscaler{
				Spec: SpannerAutoscalerSpec{
					ScaleTargetRef: ScaleTargetRef{
						ProjectID:  pointer.String("src-project-id"),
						InstanceID: pointer.String("src-instance-id"),
					},
					TargetCPUUtilization: TargetCPUUtilization{
						HighPriority: pointer.Int32(50),
					},
				},
			},
			expected: &v1beta1.SpannerAutoscaler{
				Spec: v1beta1.SpannerAutoscalerSpec{
					TargetInstance: v1beta1.TargetInstance{
						ProjectID:  "src-project-id",
						InstanceID: "src-instance-id",
					},
					ScaleConfig: v1beta1.ScaleConfig{
						TargetCPUUtilization: v1beta1.TargetCPUUtilization{
							HighPriority: 50,
						},
					},
				},
			},
		},
		{
			title: "authentication AuthTypeImpersonation",
			src: &SpannerAutoscaler{
				Spec: SpannerAutoscalerSpec{
					ScaleTargetRef: ScaleTargetRef{
						ProjectID:  pointer.String("src-project-id"),
						InstanceID: pointer.String("src-instance-id"),
					},
					ImpersonateConfig: &ImpersonateConfig{
						TargetServiceAccount: "src-target-service-account",
						Delegates:            []string{"src-delegate1", "src-delegate2"},
					},
					TargetCPUUtilization: TargetCPUUtilization{
						HighPriority: pointer.Int32(50),
					},
				},
			},
			expected: &v1beta1.SpannerAutoscaler{
				Spec: v1beta1.SpannerAutoscalerSpec{
					TargetInstance: v1beta1.TargetInstance{
						ProjectID:  "src-project-id",
						InstanceID: "src-instance-id",
					},
					Authentication: v1beta1.Authentication{
						Type: v1beta1.AuthTypeImpersonation,
						ImpersonateConfig: &v1beta1.ImpersonateConfig{
							TargetServiceAccount: "src-target-service-account",
							Delegates:            []string{"src-delegate1", "src-delegate2"},
						},
					},
					ScaleConfig: v1beta1.ScaleConfig{
						TargetCPUUtilization: v1beta1.TargetCPUUtilization{
							HighPriority: 50,
						},
					},
				},
			},
		},
		{
			title: "authentication AuthTypeSA with Namespace",
			src: &SpannerAutoscaler{
				Spec: SpannerAutoscalerSpec{
					ScaleTargetRef: ScaleTargetRef{
						ProjectID:  pointer.String("src-project-id"),
						InstanceID: pointer.String("src-instance-id"),
					},
					ServiceAccountSecretRef: &ServiceAccountSecretRef{
						Name:      pointer.String("src-service-account-secret-name"),
						Namespace: pointer.String("src-service-account-secret-namespace"),
						Key:       pointer.String("src-service-account-secret-key"),
					},
					TargetCPUUtilization: TargetCPUUtilization{
						HighPriority: pointer.Int32(50),
					},
				},
			},
			expected: &v1beta1.SpannerAutoscaler{
				Spec: v1beta1.SpannerAutoscalerSpec{
					TargetInstance: v1beta1.TargetInstance{
						ProjectID:  "src-project-id",
						InstanceID: "src-instance-id",
					},
					Authentication: v1beta1.Authentication{
						Type: v1beta1.AuthTypeSA,
						IAMKeySecret: &v1beta1.IAMKeySecret{
							Name:      "src-service-account-secret-name",
							Namespace: "src-service-account-secret-namespace",
							Key:       "src-service-account-secret-key",
						},
					},
					ScaleConfig: v1beta1.ScaleConfig{
						TargetCPUUtilization: v1beta1.TargetCPUUtilization{
							HighPriority: 50,
						},
					},
				},
			},
		},
		{
			title: "authentication AuthTypeSA without Namespace",
			src: &SpannerAutoscaler{
				Spec: SpannerAutoscalerSpec{
					ScaleTargetRef: ScaleTargetRef{
						ProjectID:  pointer.String("src-project-id"),
						InstanceID: pointer.String("src-instance-id"),
					},
					ServiceAccountSecretRef: &ServiceAccountSecretRef{
						Name: pointer.String("src-service-account-secret-name"),
						Key:  pointer.String("src-service-account-secret-key"),
					},
					TargetCPUUtilization: TargetCPUUtilization{
						HighPriority: pointer.Int32(50),
					},
				},
			},
			expected: &v1beta1.SpannerAutoscaler{
				Spec: v1beta1.SpannerAutoscalerSpec{
					TargetInstance: v1beta1.TargetInstance{
						ProjectID:  "src-project-id",
						InstanceID: "src-instance-id",
					},
					Authentication: v1beta1.Authentication{
						Type: v1beta1.AuthTypeSA,
						IAMKeySecret: &v1beta1.IAMKeySecret{
							Name: "src-service-account-secret-name",
							Key:  "src-service-account-secret-key",
						},
					},
					ScaleConfig: v1beta1.ScaleConfig{
						TargetCPUUtilization: v1beta1.TargetCPUUtilization{
							HighPriority: 50,
						},
					},
				},
			},
		},
		{
			title: "scaleConfig ComputeTypeNode",
			src: &SpannerAutoscaler{
				Spec: SpannerAutoscalerSpec{
					ScaleTargetRef: ScaleTargetRef{
						ProjectID:  pointer.String("src-project-id"),
						InstanceID: pointer.String("src-instance-id"),
					},
					MinNodes:          pointer.Int32(1),
					MaxNodes:          pointer.Int32(10),
					MaxScaleDownNodes: pointer.Int32(1),
					TargetCPUUtilization: TargetCPUUtilization{
						HighPriority: pointer.Int32(50),
					},
				},
			},
			expected: &v1beta1.SpannerAutoscaler{
				Spec: v1beta1.SpannerAutoscalerSpec{
					TargetInstance: v1beta1.TargetInstance{
						ProjectID:  "src-project-id",
						InstanceID: "src-instance-id",
					},
					ScaleConfig: v1beta1.ScaleConfig{
						ComputeType: v1beta1.ComputeTypeNode,
						Nodes: v1beta1.ScaleConfigNodes{
							Min: 1,
							Max: 10,
						},
						ScaledownStepSize: 1,
						TargetCPUUtilization: v1beta1.TargetCPUUtilization{
							HighPriority: 50,
						},
					},
				},
			},
		},
		{
			title: "scaleConfig ComputeTypePU",
			src: &SpannerAutoscaler{
				Spec: SpannerAutoscalerSpec{
					ScaleTargetRef: ScaleTargetRef{
						ProjectID:  pointer.String("src-project-id"),
						InstanceID: pointer.String("src-instance-id"),
					},
					MinProcessingUnits: pointer.Int32(100),
					MaxProcessingUnits: pointer.Int32(1000),
					MaxScaleDownNodes:  pointer.Int32(1),
					TargetCPUUtilization: TargetCPUUtilization{
						HighPriority: pointer.Int32(50),
					},
				},
			},
			expected: &v1beta1.SpannerAutoscaler{
				Spec: v1beta1.SpannerAutoscalerSpec{
					TargetInstance: v1beta1.TargetInstance{
						ProjectID:  "src-project-id",
						InstanceID: "src-instance-id",
					},
					ScaleConfig: v1beta1.ScaleConfig{
						ComputeType: v1beta1.ComputeTypePU,
						ProcessingUnits: v1beta1.ScaleConfigPUs{
							Min: 100,
							Max: 1000,
						},
						ScaledownStepSize: 1,
						TargetCPUUtilization: v1beta1.TargetCPUUtilization{
							HighPriority: 50,
						},
					},
				},
			},
		},
		{
			title: "objectMeta",
			src: &SpannerAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "src-name",
					Namespace: "src-namespace",
				},
				Spec: SpannerAutoscalerSpec{
					ScaleTargetRef: ScaleTargetRef{
						ProjectID:  pointer.String("src-project-id"),
						InstanceID: pointer.String("src-instance-id"),
					},
					TargetCPUUtilization: TargetCPUUtilization{
						HighPriority: pointer.Int32(50),
					},
				},
			},
			expected: &v1beta1.SpannerAutoscaler{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "src-name",
					Namespace: "src-namespace",
				},
				Spec: v1beta1.SpannerAutoscalerSpec{
					TargetInstance: v1beta1.TargetInstance{
						ProjectID:  "src-project-id",
						InstanceID: "src-instance-id",
					},
					ScaleConfig: v1beta1.ScaleConfig{
						TargetCPUUtilization: v1beta1.TargetCPUUtilization{
							HighPriority: 50,
						},
					},
				},
			},
		},
		{
			title: "status",
			src: &SpannerAutoscaler{
				Status: SpannerAutoscalerStatus{
					LastScaleTime:                     &metav1.Time{Time: timestamp},
					LastSyncTime:                      &metav1.Time{Time: timestamp.Add(-1 * time.Hour)},
					CurrentNodes:                      pointer.Int32(1),
					CurrentProcessingUnits:            pointer.Int32(100),
					DesiredNodes:                      pointer.Int32(2),
					DesiredProcessingUnits:            pointer.Int32(200),
					CurrentHighPriorityCPUUtilization: pointer.Int32(10),
					InstanceState:                     InstanceStateReady,
				},
				Spec: SpannerAutoscalerSpec{
					ScaleTargetRef: ScaleTargetRef{
						ProjectID:  pointer.String("src-project-id"),
						InstanceID: pointer.String("src-instance-id"),
					},
					TargetCPUUtilization: TargetCPUUtilization{
						HighPriority: pointer.Int32(50),
					},
				},
			},
			expected: &v1beta1.SpannerAutoscaler{
				Status: v1beta1.SpannerAutoscalerStatus{
					LastScaleTime:                     metav1.Time{Time: timestamp},
					LastSyncTime:                      metav1.Time{Time: timestamp.Add(-1 * time.Hour)},
					CurrentNodes:                      1,
					CurrentProcessingUnits:            100,
					DesiredNodes:                      2,
					DesiredProcessingUnits:            200,
					CurrentHighPriorityCPUUtilization: 10,
					InstanceState:                     v1beta1.InstanceStateReady,
				},
				Spec: v1beta1.SpannerAutoscalerSpec{
					TargetInstance: v1beta1.TargetInstance{
						ProjectID:  "src-project-id",
						InstanceID: "src-instance-id",
					},
					ScaleConfig: v1beta1.ScaleConfig{
						TargetCPUUtilization: v1beta1.TargetCPUUtilization{
							HighPriority: 50,
						},
					},
				},
			},
		},
	}

	for _, tc := range tests {
		tc := tc

		t.Run(tc.title, func(t *testing.T) {
			dest := &v1beta1.SpannerAutoscaler{}
			err := tc.src.ConvertTo(dest)
			require.NoError(t, err)

			require.Equal(t, tc.expected, dest)
		})
	}
}
