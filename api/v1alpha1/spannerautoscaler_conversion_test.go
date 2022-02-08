package v1alpha1

import (
	"github.com/mercari/spanner-autoscaler/api/v1beta1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
	"time"
)

var _ = Describe("ConvertTo", func() {
	var dest *v1beta1.SpannerAutoscaler
	var src *SpannerAutoscaler
	var expected *v1beta1.SpannerAutoscaler
	var timestamp time.Time

	BeforeEach(func() {
		dest = &v1beta1.SpannerAutoscaler{}
		src = &SpannerAutoscaler{
			Spec: SpannerAutoscalerSpec{
				ScaleTargetRef: ScaleTargetRef{
					ProjectID:  pointer.String("src-project-id"),
					InstanceID: pointer.String("src-instance-id"),
				},
				TargetCPUUtilization: TargetCPUUtilization{
					HighPriority: pointer.Int32(50),
				},
			},
		}
		expected = &v1beta1.SpannerAutoscaler{
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
		}
		timestamp = time.Now()
	})

	DescribeTable("authentication",
		func(impersonateConfig *ImpersonateConfig, serviceAccountSecretRef *ServiceAccountSecretRef, expectedAuthentication v1beta1.Authentication) {
			src.Spec.ImpersonateConfig = impersonateConfig
			src.Spec.ServiceAccountSecretRef = serviceAccountSecretRef

			expected.Spec.Authentication = expectedAuthentication

			err := src.ConvertTo(dest)
			Expect(err).NotTo(HaveOccurred())
			Expect(dest).To(Equal(expected))
		},
		Entry("AuthType is impersonation",
			&ImpersonateConfig{
				TargetServiceAccount: "src-target-service-account",
				Delegates:            []string{"src-delegate1", "src-delegate2"},
			},
			nil,
			v1beta1.Authentication{
				Type: v1beta1.AuthTypeImpersonation,
				ImpersonateConfig: &v1beta1.ImpersonateConfig{
					TargetServiceAccount: "src-target-service-account",
					Delegates:            []string{"src-delegate1", "src-delegate2"},
				},
			},
		),
		Entry("AuthType is SA with Namespace",
			nil,
			&ServiceAccountSecretRef{
				Name:      pointer.String("src-service-account-secret-name"),
				Namespace: pointer.String("src-service-account-secret-namespace"),
				Key:       pointer.String("src-service-account-secret-key"),
			},
			v1beta1.Authentication{
				Type: v1beta1.AuthTypeSA,
				IAMKeySecret: &v1beta1.IAMKeySecret{
					Name:      "src-service-account-secret-name",
					Namespace: "src-service-account-secret-namespace",
					Key:       "src-service-account-secret-key",
				},
			},
		),
		Entry("AuthType is SA without Namespace",
			nil,
			&ServiceAccountSecretRef{
				Name: pointer.String("src-service-account-secret-name"),
				Key:  pointer.String("src-service-account-secret-key"),
			},
			v1beta1.Authentication{
				Type: v1beta1.AuthTypeSA,
				IAMKeySecret: &v1beta1.IAMKeySecret{
					Name: "src-service-account-secret-name",
					Key:  "src-service-account-secret-key",
				},
			},
		),
	)

	DescribeTable("scaleConfig",
		func(minNodes, maxNodes, minPU, maxPU, maxScaleDownNodes *int32, expectedScaleConfig v1beta1.ScaleConfig) {
			src.Spec.MinNodes = minNodes
			src.Spec.MaxNodes = maxNodes
			src.Spec.MinProcessingUnits = minPU
			src.Spec.MaxProcessingUnits = maxPU
			src.Spec.MaxScaleDownNodes = maxScaleDownNodes

			expected.Spec.ScaleConfig = expectedScaleConfig

			err := src.ConvertTo(dest)
			Expect(err).NotTo(HaveOccurred())
			Expect(dest).To(Equal(expected))
		},
		Entry("ComputeType is Node", pointer.Int32(1), pointer.Int32(10), nil, nil, pointer.Int32(1),
			v1beta1.ScaleConfig{
				ComputeType: v1beta1.ComputeTypeNode,
				Nodes: v1beta1.ScaleConfigNodes{
					Min: 1,
					Max: 10,
				},
				ScaledownStepSize: 1000,
				TargetCPUUtilization: v1beta1.TargetCPUUtilization{
					HighPriority: 50,
				},
			},
		),
		Entry("ComputeType is PU", nil, nil, pointer.Int32(100), pointer.Int32(1000), pointer.Int32(1),
			v1beta1.ScaleConfig{
				ComputeType: v1beta1.ComputeTypePU,
				ProcessingUnits: v1beta1.ScaleConfigPUs{
					Min: 100,
					Max: 1000,
				},
				ScaledownStepSize: 1000,
				TargetCPUUtilization: v1beta1.TargetCPUUtilization{
					HighPriority: 50,
				},
			},
		),
	)

	DescribeTable("objectMeta",
		func(objectMeta metav1.ObjectMeta) {
			src.ObjectMeta = objectMeta
			expected.ObjectMeta = objectMeta

			err := src.ConvertTo(dest)
			Expect(err).NotTo(HaveOccurred())
			Expect(dest).To(Equal(expected))
		},
		Entry("Name and Namespace", metav1.ObjectMeta{
			Name:      "src-name",
			Namespace: "src-namespace",
		}),
	)

	DescribeTable("status",
		func(status SpannerAutoscalerStatus, expectedStatus v1beta1.SpannerAutoscalerStatus) {
			src.Status = status
			expected.Status = expectedStatus

			err := src.ConvertTo(dest)
			Expect(err).NotTo(HaveOccurred())
			Expect(dest).To(Equal(expected))
		},
		Entry("All statuses", SpannerAutoscalerStatus{
			LastScaleTime:                     &metav1.Time{Time: timestamp},
			LastSyncTime:                      &metav1.Time{Time: timestamp.Add(-1 * time.Hour)},
			CurrentNodes:                      pointer.Int32(1),
			CurrentProcessingUnits:            pointer.Int32(100),
			DesiredNodes:                      pointer.Int32(2),
			DesiredProcessingUnits:            pointer.Int32(200),
			CurrentHighPriorityCPUUtilization: pointer.Int32(10),
			InstanceState:                     InstanceStateReady,
		}, v1beta1.SpannerAutoscalerStatus{
			LastScaleTime:                     metav1.Time{Time: timestamp},
			LastSyncTime:                      metav1.Time{Time: timestamp.Add(-1 * time.Hour)},
			CurrentNodes:                      1,
			CurrentProcessingUnits:            100,
			DesiredNodes:                      2,
			DesiredProcessingUnits:            200,
			CurrentHighPriorityCPUUtilization: 10,
			InstanceState:                     v1beta1.InstanceStateReady,
		}),
	)
})

var _ = Describe("ConvertFrom", func() {
	var dest *SpannerAutoscaler
	var src *v1beta1.SpannerAutoscaler

	BeforeEach(func() {
		dest = &SpannerAutoscaler{}
		src = &v1beta1.SpannerAutoscaler{
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
		}
	})

	DescribeTable("authentication",
		func(authentication v1beta1.Authentication, expectedServiceAccountSecretRef *ServiceAccountSecretRef, expectedImpersonateConfig *ImpersonateConfig) {
			src.Spec.Authentication = authentication

			err := dest.ConvertFrom(src)
			Expect(err).NotTo(HaveOccurred())
			Expect(dest.Spec.ServiceAccountSecretRef).To(Equal(expectedServiceAccountSecretRef))
			Expect(dest.Spec.ImpersonateConfig).To(Equal(expectedImpersonateConfig))
		},
		Entry("AuthType is SA",
			v1beta1.Authentication{
				Type: v1beta1.AuthTypeSA,
				IAMKeySecret: &v1beta1.IAMKeySecret{
					Name:      "src-service-account-secret-name",
					Namespace: "src-service-account-secret-namespace",
					Key:       "src-service-account-secret-key",
				},
			},
			&ServiceAccountSecretRef{
				Name:      pointer.String("src-service-account-secret-name"),
				Namespace: pointer.String("src-service-account-secret-namespace"),
				Key:       pointer.String("src-service-account-secret-key"),
			},
			nil,
		),
		Entry("AuthType is impersonation",
			v1beta1.Authentication{
				Type: v1beta1.AuthTypeImpersonation,
				ImpersonateConfig: &v1beta1.ImpersonateConfig{
					TargetServiceAccount: "src-target-service-account",
					Delegates:            []string{"src-delegate1", "src-delegate2"},
				},
			},
			nil,
			&ImpersonateConfig{
				TargetServiceAccount: "src-target-service-account",
				Delegates:            []string{"src-delegate1", "src-delegate2"},
			},
		),
	)

	DescribeTable("scaleConfig",
		func(scaleConfig v1beta1.ScaleConfig, expectedMinNodes, expectedMaxNodes, expectedMinPU, expectedMaxPU *int32) {
			src.Spec.ScaleConfig = scaleConfig
			src.Spec.ScaleConfig.ScaledownStepSize = 1000
			src.Spec.ScaleConfig.TargetCPUUtilization = v1beta1.TargetCPUUtilization{
				HighPriority: 50,
			}

			err := dest.ConvertFrom(src)
			Expect(err).NotTo(HaveOccurred())
			Expect(dest.Spec.MinNodes).To(Equal(expectedMinNodes))
			Expect(dest.Spec.MaxNodes).To(Equal(expectedMaxNodes))
			Expect(dest.Spec.MinProcessingUnits).To(Equal(expectedMinPU))
			Expect(dest.Spec.MaxProcessingUnits).To(Equal(expectedMaxPU))
			Expect(dest.Spec.MaxScaleDownNodes).To(Equal(pointer.Int32(1)))
			Expect(dest.Spec.TargetCPUUtilization.HighPriority).To(Equal(pointer.Int32(50)))
		},
		Entry("ComputeType is Node",
			v1beta1.ScaleConfig{
				ComputeType: v1beta1.ComputeTypeNode,
				Nodes: v1beta1.ScaleConfigNodes{
					Min: 1,
					Max: 10,
				},
			},
			pointer.Int32(1), pointer.Int32(10), nil, nil,
		),
		Entry("ComputeType is PU",
			v1beta1.ScaleConfig{
				ComputeType: v1beta1.ComputeTypePU,
				ProcessingUnits: v1beta1.ScaleConfigPUs{
					Min: 100,
					Max: 1000,
				},
			},
			nil, nil, pointer.Int32(100), pointer.Int32(1000),
		),
	)

	Describe("objectMeta", func() {
		It("should convert name and namespace fields", func() {
			objectMeta := metav1.ObjectMeta{
				Name:      "src-name",
				Namespace: "src-namespace",
			}

			src.ObjectMeta = objectMeta

			err := dest.ConvertFrom(src)
			Expect(err).NotTo(HaveOccurred())
			Expect(dest.ObjectMeta).To(Equal(objectMeta))
		})
	})

	Describe("status", func() {
		It("should convert all the status fields", func() {
			timestamp := time.Now()

			src.Status = v1beta1.SpannerAutoscalerStatus{
				LastScaleTime:                     metav1.Time{Time: timestamp},
				LastSyncTime:                      metav1.Time{Time: timestamp.Add(-1 * time.Hour)},
				CurrentNodes:                      1,
				CurrentProcessingUnits:            100,
				DesiredNodes:                      2,
				DesiredProcessingUnits:            200,
				CurrentHighPriorityCPUUtilization: 10,
				InstanceState:                     v1beta1.InstanceStateReady,
			}

			expectedStatus := SpannerAutoscalerStatus{
				LastScaleTime:                     &metav1.Time{Time: timestamp},
				LastSyncTime:                      &metav1.Time{Time: timestamp.Add(-1 * time.Hour)},
				CurrentNodes:                      pointer.Int32(1),
				CurrentProcessingUnits:            pointer.Int32(100),
				DesiredNodes:                      pointer.Int32(2),
				DesiredProcessingUnits:            pointer.Int32(200),
				CurrentHighPriorityCPUUtilization: pointer.Int32(10),
				InstanceState:                     InstanceStateReady,
			}

			err := dest.ConvertFrom(src)
			Expect(err).NotTo(HaveOccurred())
			Expect(dest.Status).To(Equal(expectedStatus))
		})
	})
})
