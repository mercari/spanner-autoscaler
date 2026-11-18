package v1alpha1

import (
	"time"

	"github.com/mercari/spanner-autoscaler/api/v1beta1"
	"github.com/mercari/spanner-autoscaler/internal/scaling"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
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
					ProjectID:  ptr.To("src-project-id"),
					InstanceID: ptr.To("src-instance-id"),
				},
				TargetCPUUtilization: TargetCPUUtilization{
					HighPriority: ptr.To[int32](50),
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
						HighPriority: intPtr(50),
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
				Name:      ptr.To("src-service-account-secret-name"),
				Namespace: ptr.To("src-service-account-secret-namespace"),
				Key:       ptr.To("src-service-account-secret-key"),
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
				Name: ptr.To("src-service-account-secret-name"),
				Key:  ptr.To("src-service-account-secret-key"),
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
		Entry("ComputeType is Node", ptr.To[int32](1), ptr.To[int32](10), nil, nil, ptr.To[int32](1),
			v1beta1.ScaleConfig{
				ComputeType: v1beta1.ComputeTypeNode,
				Nodes: v1beta1.ScaleConfigNodes{
					Min: 1,
					Max: 10,
				},
				ScaledownStepSize: intstr.FromInt(1000),
				TargetCPUUtilization: v1beta1.TargetCPUUtilization{
					HighPriority: intPtr(50),
				},
			},
		),
		Entry("ComputeType is PU", nil, nil, ptr.To[int32](100), ptr.To[int32](1000), ptr.To[int32](1),
			v1beta1.ScaleConfig{
				ComputeType: v1beta1.ComputeTypePU,
				ProcessingUnits: v1beta1.ScaleConfigPUs{
					Min: 100,
					Max: 1000,
				},
				ScaledownStepSize: intstr.FromInt(1000),
				TargetCPUUtilization: v1beta1.TargetCPUUtilization{
					HighPriority: intPtr(50),
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
			CurrentNodes:                      ptr.To[int32](0),
			CurrentProcessingUnits:            ptr.To[int32](100),
			DesiredNodes:                      ptr.To[int32](0),
			DesiredProcessingUnits:            ptr.To[int32](200),
			CurrentHighPriorityCPUUtilization: ptr.To[int32](10),
			InstanceState:                     InstanceStateReady,
		}, v1beta1.SpannerAutoscalerStatus{
			LastScaleTime:                     metav1.Time{Time: timestamp},
			LastSyncTime:                      metav1.Time{Time: timestamp.Add(-1 * time.Hour)},
			CurrentProcessingUnits:            100,
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
						HighPriority: intPtr(50),
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
				Name:      ptr.To("src-service-account-secret-name"),
				Namespace: ptr.To("src-service-account-secret-namespace"),
				Key:       ptr.To("src-service-account-secret-key"),
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
			src.Spec.ScaleConfig.ScaledownStepSize = intstr.FromInt(1000)
			src.Spec.ScaleConfig.TargetCPUUtilization = v1beta1.TargetCPUUtilization{
				HighPriority: intPtr(50),
			}

			err := dest.ConvertFrom(src)
			Expect(err).NotTo(HaveOccurred())
			Expect(dest.Spec.MinNodes).To(Equal(expectedMinNodes))
			Expect(dest.Spec.MaxNodes).To(Equal(expectedMaxNodes))
			Expect(dest.Spec.MinProcessingUnits).To(Equal(expectedMinPU))
			Expect(dest.Spec.MaxProcessingUnits).To(Equal(expectedMaxPU))
			Expect(dest.Spec.MaxScaleDownNodes).To(Equal(ptr.To[int32](1)))
			Expect(dest.Spec.TargetCPUUtilization.HighPriority).To(Equal(ptr.To[int32](50)))
		},
		Entry("ComputeType is Node",
			v1beta1.ScaleConfig{
				ComputeType: v1beta1.ComputeTypeNode,
				Nodes: v1beta1.ScaleConfigNodes{
					Min: 1,
					Max: 10,
				},
			},
			ptr.To[int32](1), ptr.To[int32](10), nil, nil,
		),
		Entry("ComputeType is PU",
			v1beta1.ScaleConfig{
				ComputeType: v1beta1.ComputeTypePU,
				ProcessingUnits: v1beta1.ScaleConfigPUs{
					Min: 100,
					Max: 1000,
				},
			},
			nil, nil, ptr.To[int32](100), ptr.To[int32](1000),
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
				CurrentProcessingUnits:            100,
				DesiredProcessingUnits:            200,
				CurrentHighPriorityCPUUtilization: 10,
				InstanceState:                     v1beta1.InstanceStateReady,
			}

			expectedStatus := SpannerAutoscalerStatus{
				LastScaleTime:                     &metav1.Time{Time: timestamp},
				LastSyncTime:                      &metav1.Time{Time: timestamp.Add(-1 * time.Hour)},
				CurrentNodes:                      ptr.To[int32](0),
				CurrentProcessingUnits:            ptr.To[int32](100),
				DesiredNodes:                      ptr.To[int32](0),
				DesiredProcessingUnits:            ptr.To[int32](200),
				CurrentHighPriorityCPUUtilization: ptr.To[int32](10),
				InstanceState:                     InstanceStateReady,
			}

			err := dest.ConvertFrom(src)
			Expect(err).NotTo(HaveOccurred())
			Expect(dest.Status).To(Equal(expectedStatus))
		})
	})
})

func intPtr(i int) *int { return &i }

var _ = Describe("timeBasedOverrides round trip", func() {
	It("should keep the overrides through v1alpha1 without leaking the annotation", func() {
		overrides := []v1beta1.TimeBasedOverride{
			{
				Name:              "night",
				Times:             []string{"CRON_TZ=Asia/Tokyo * 1-6 * * *"},
				ProcessingUnits:   &v1beta1.TimeBasedOverridePUs{Min: new(8000)},
				ScaledownStepSize: new(intstr.FromString("30%")),
			},
			{Name: "others", Times: []string{"* * * * *"}, ScaledownPolicy: v1beta1.ScaledownPolicyDeny},
		}
		src := &v1beta1.SpannerAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "ns", Annotations: map[string]string{"keep": "me"}},
			Spec: v1beta1.SpannerAutoscalerSpec{
				TargetInstance: v1beta1.TargetInstance{ProjectID: "p", InstanceID: "i"},
				ScaleConfig: v1beta1.ScaleConfig{
					ComputeType:          v1beta1.ComputeTypePU,
					ProcessingUnits:      v1beta1.ScaleConfigPUs{Min: 12000, Max: 80000},
					ScaledownStepSize:    intstr.FromInt(2000),
					TargetCPUUtilization: v1beta1.TargetCPUUtilization{HighPriority: new(50)},
					TimeBasedOverrides:   overrides,
				},
			},
		}

		alpha := &SpannerAutoscaler{}
		Expect(alpha.ConvertFrom(src)).To(Succeed())
		Expect(alpha.Annotations).To(HaveKey(scaleConfigAnnotation))
		Expect(src.Annotations).To(Equal(map[string]string{"keep": "me"}), "the source annotations must not be modified")

		back := &v1beta1.SpannerAutoscaler{}
		Expect(alpha.ConvertTo(back)).To(Succeed())
		Expect(back.Spec.ScaleConfig.TimeBasedOverrides).To(Equal(overrides))
		Expect(back.Annotations).To(Equal(map[string]string{"keep": "me"}))
		Expect(alpha.Annotations).To(HaveKey(scaleConfigAnnotation), "the v1alpha1 object must not be modified")
	})

	Describe("base scale config that v1alpha1 cannot hold", func() {
		newSrc := func() *v1beta1.SpannerAutoscaler {
			return &v1beta1.SpannerAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "ns"},
				Spec: v1beta1.SpannerAutoscalerSpec{
					TargetInstance: v1beta1.TargetInstance{ProjectID: "p", InstanceID: "i"},
					ScaleConfig: v1beta1.ScaleConfig{
						ComputeType:          v1beta1.ComputeTypePU,
						ProcessingUnits:      v1beta1.ScaleConfigPUs{Min: 12000, Max: 80000},
						ScaledownStepSize:    intstr.FromString("10%"),
						ScaleupStepSize:      intstr.FromInt(4000),
						ScaledownInterval:    &metav1.Duration{Duration: 30 * time.Minute},
						TargetCPUUtilization: v1beta1.TargetCPUUtilization{HighPriority: new(50), Total: new(80)},
						TimeBasedOverrides: []v1beta1.TimeBasedOverride{{
							Name:                 "night",
							Times:                []string{"* * * * *"},
							TargetCPUUtilization: &v1beta1.TargetCPUUtilization{Total: new(90)},
						}},
					},
				},
			}
		}

		It("should restore the base fields so the overrides stay valid", func() {
			src := newSrc()
			alpha := &SpannerAutoscaler{}
			Expect(alpha.ConvertFrom(src)).To(Succeed())

			back := &v1beta1.SpannerAutoscaler{}
			Expect(alpha.ConvertTo(back)).To(Succeed())
			Expect(back.Spec.ScaleConfig).To(Equal(src.Spec.ScaleConfig))
			_, err := scaling.ResolveScaleConfig(back.Spec.ScaleConfig, time.Now())
			Expect(err).NotTo(HaveOccurred())
		})

		It("should take the fields a v1alpha1 client changed and keep the rest", func() {
			src := newSrc()
			alpha := &SpannerAutoscaler{}
			Expect(alpha.ConvertFrom(src)).To(Succeed())
			alpha.Spec.MaxProcessingUnits = new(int32(90000))
			alpha.Spec.TargetCPUUtilization.HighPriority = new(int32(60))

			back := &v1beta1.SpannerAutoscaler{}
			Expect(alpha.ConvertTo(back)).To(Succeed())
			want := src.Spec.ScaleConfig
			want.ProcessingUnits.Max = 90000
			want.TargetCPUUtilization.HighPriority = new(60)
			Expect(back.Spec.ScaleConfig).To(Equal(want))
		})

		// Overrides are in processing units for both compute types: the
		// defaulter derives processingUnits from nodes, and the webhook and
		// ResolveScaleConfig work on processingUnits. A compute-type switch
		// therefore keeps the overrides meaningful.
		It("should keep the overrides valid when a v1alpha1 client switches to nodes", func() {
			src := newSrc()
			src.Spec.ScaleConfig.TimeBasedOverrides[0].ProcessingUnits = &v1beta1.TimeBasedOverridePUs{Max: new(50000)}
			alpha := &SpannerAutoscaler{}
			Expect(alpha.ConvertFrom(src)).To(Succeed())
			alpha.Spec.MinProcessingUnits = nil
			alpha.Spec.MaxProcessingUnits = nil
			alpha.Spec.MinNodes = new(int32(12))
			alpha.Spec.MaxNodes = new(int32(80))

			back := &v1beta1.SpannerAutoscaler{}
			Expect(alpha.ConvertTo(back)).To(Succeed())
			sc := back.Spec.ScaleConfig
			Expect(sc.ComputeType).To(Equal(v1beta1.ComputeTypeNode))
			Expect(sc.Nodes).To(Equal(v1beta1.ScaleConfigNodes{Min: 12, Max: 80}))
			Expect(sc.TimeBasedOverrides).To(Equal(src.Spec.ScaleConfig.TimeBasedOverrides))

			// What the defaulting webhook does for computeType node.
			sc.ProcessingUnits = v1beta1.ScaleConfigPUs{Min: sc.Nodes.Min * 1000, Max: sc.Nodes.Max * 1000}
			resolved, err := scaling.ResolveScaleConfig(sc, time.Now())
			Expect(err).NotTo(HaveOccurred())
			Expect(resolved.ScaleConfig.ProcessingUnits).To(Equal(v1beta1.ScaleConfigPUs{Min: 12000, Max: 50000}))
		})
	})

	It("should drop a stale annotation when the v1beta1 object has no overrides", func() {
		annotations := map[string]string{
			scaleConfigAnnotation: `{"processingUnits":{"min":8000,"max":80000},"scaledownStepSize":"30%",` +
				`"targetCPUUtilization":{"highPriority":70},` +
				`"timeBasedOverrides":[{"name":"others","times":["* * * * *"],"scaledownPolicy":"Deny"}]}`,
			"keep": "me",
		}

		src := &v1beta1.SpannerAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "n", Namespace: "ns", Annotations: annotations},
			Spec: v1beta1.SpannerAutoscalerSpec{
				TargetInstance: v1beta1.TargetInstance{ProjectID: "p", InstanceID: "i"},
				ScaleConfig: v1beta1.ScaleConfig{
					ComputeType:          v1beta1.ComputeTypePU,
					ProcessingUnits:      v1beta1.ScaleConfigPUs{Min: 1000, Max: 5000},
					ScaledownStepSize:    intstr.FromInt(2000),
					TargetCPUUtilization: v1beta1.TargetCPUUtilization{HighPriority: new(50)},
				},
			},
		}
		alpha := &SpannerAutoscaler{}
		Expect(alpha.ConvertFrom(src)).To(Succeed())
		Expect(alpha.Annotations).To(Equal(map[string]string{"keep": "me"}))
		Expect(src.Annotations).To(HaveKey(scaleConfigAnnotation), "the v1beta1 object must not be modified")

		back := &v1beta1.SpannerAutoscaler{}
		Expect(alpha.ConvertTo(back)).To(Succeed())
		Expect(back.Spec.ScaleConfig).To(Equal(src.Spec.ScaleConfig))
	})

	It("should fail on a broken annotation instead of dropping the overrides", func() {
		alpha := &SpannerAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{scaleConfigAnnotation: "{"}},
			Spec: SpannerAutoscalerSpec{
				ScaleTargetRef:       ScaleTargetRef{ProjectID: new("p"), InstanceID: new("i")},
				TargetCPUUtilization: TargetCPUUtilization{HighPriority: new(int32(50))},
			},
		}
		Expect(alpha.ConvertTo(&v1beta1.SpannerAutoscaler{})).NotTo(Succeed())
	})
})
