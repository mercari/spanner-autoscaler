package v1beta1

import (
	"fmt"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
)

var _ = Describe("SpannerAutoscaler validation", func() {
	var (
		testResource *spannerv1beta1.SpannerAutoscaler
		namespace    string
		name         string
	)

	BeforeEach(func() {
		name = "test-spanner-autoscaler"
		namespace = uuid.NewString()
		err := k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namespace,
			},
		})
		Expect(err).NotTo(HaveOccurred())

		testResource = &spannerv1beta1.SpannerAutoscaler{
			TypeMeta: metav1.TypeMeta{
				Kind:       "SpannerAutoscaler",
				APIVersion: "spanner.mercari.com/v1beta1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: spannerv1beta1.SpannerAutoscalerSpec{
				TargetInstance: spannerv1beta1.TargetInstance{
					ProjectID:  "test-project-id",
					InstanceID: "test-instance-id",
				},
				ScaleConfig: spannerv1beta1.ScaleConfig{
					TargetCPUUtilization: spannerv1beta1.TargetCPUUtilization{
						HighPriority: intPtr(30),
					},
				},
			},
		}
	})

	Describe("Check default settings", func() {
		Describe("Authentication", func() {
			BeforeEach(func() {
				testResource.Spec.ScaleConfig.ProcessingUnits = spannerv1beta1.ScaleConfigPUs{
					Min: 1000,
					Max: 10000,
				}
			})

			Context("IAM key secret is set", func() {
				BeforeEach(func() {
					testResource.Spec.Authentication = spannerv1beta1.Authentication{
						IAMKeySecret: &spannerv1beta1.IAMKeySecret{
							Name: "test-service-account-secret",
							Key:  "secret",
						},
					}
				})

				Context("namespace exists", func() {
					BeforeEach(func() {
						testResource.Spec.Authentication.IAMKeySecret.Namespace = "default"
					})

					It("should set authentication type 'gcp-sa-key' automatically", func() {
						result, err := createResource(testResource)
						Expect(err).ToNot(HaveOccurred())
						Expect(result.Spec.Authentication.Type).To(Equal(spannerv1beta1.AuthTypeSA))
						Expect(result.Spec.Authentication.IAMKeySecret.Namespace).To(Equal("default"))
					})
				})

				Context("namespace does not exist", func() {
					It("should set authentication type 'gcp-sa-key' and namespace automatically", func() {
						result, err := createResource(testResource)
						Expect(err).ToNot(HaveOccurred())
						Expect(result.Spec.Authentication.Type).To(Equal(spannerv1beta1.AuthTypeSA))
						Expect(result.Spec.Authentication.IAMKeySecret.Namespace).To(Equal(namespace))
					})
				})
			})

			Context("impersonate config is set", func() {
				BeforeEach(func() {
					testResource.Spec.Authentication = spannerv1beta1.Authentication{
						ImpersonateConfig: &spannerv1beta1.ImpersonateConfig{
							TargetServiceAccount: "test-service-account",
							Delegates:            []string{"test-delegate"},
						},
					}
				})

				It("should set authentication type 'impersonation' automatically", func() {
					result, err := createResource(testResource)
					Expect(err).ToNot(HaveOccurred())
					Expect(result.Spec.Authentication.Type).To(Equal(spannerv1beta1.AuthTypeImpersonation))
				})
			})

			Context("no additional authentication config is set", func() {
				It("should set authentication type 'adc' automatically", func() {
					result, err := createResource(testResource)
					Expect(err).ToNot(HaveOccurred())
					Expect(result.Spec.Authentication.Type).To(Equal(spannerv1beta1.AuthTypeADC))
				})
			})
		})

		Describe("ScaleConfig", func() {
			Context("processing unit configuration is set", func() {
				BeforeEach(func() {
					testResource.Spec.ScaleConfig.ProcessingUnits = spannerv1beta1.ScaleConfigPUs{
						Min: 1000,
						Max: 10000,
					}
				})

				Context("scale down step size is set", func() {
					BeforeEach(func() {
						testResource.Spec.ScaleConfig.ScaledownStepSize = intstr.FromInt(1000)
					})

					It("should set compute type 'processing-units' automatically", func() {
						result, err := createResource(testResource)
						Expect(err).ToNot(HaveOccurred())
						Expect(result.Spec.ScaleConfig.ComputeType).To(Equal(spannerv1beta1.ComputeTypePU))
						Expect(result.Spec.ScaleConfig.ScaledownStepSize.IntVal).To(Equal(int32(1000)))
					})
				})

				Context("scale down step size is not set", func() {
					It("should set compute type 'processing-units' and scale down step size automatically", func() {
						result, err := createResource(testResource)
						Expect(err).ToNot(HaveOccurred())
						Expect(result.Spec.ScaleConfig.ComputeType).To(Equal(spannerv1beta1.ComputeTypePU))
						Expect(result.Spec.ScaleConfig.ScaledownStepSize.IntVal).To(Equal(int32(2000)))
					})
				})

				Context("scale down step size is set as a percentage", func() {
					BeforeEach(func() {
						testResource.Spec.ScaleConfig.ScaledownStepSize = intstr.FromString("10%")
					})

					It("should preserve the percentage and not overwrite it with the default", func() {
						result, err := createResource(testResource)
						Expect(err).ToNot(HaveOccurred())
						Expect(result.Spec.ScaleConfig.ComputeType).To(Equal(spannerv1beta1.ComputeTypePU))
						Expect(result.Spec.ScaleConfig.ScaledownStepSize.Type).To(Equal(intstr.String))
						Expect(result.Spec.ScaleConfig.ScaledownStepSize.StrVal).To(Equal("10%"))
					})
				})
			})

			Context("processing unit node is set", func() {
				BeforeEach(func() {
					testResource.Spec.ScaleConfig.Nodes = spannerv1beta1.ScaleConfigNodes{
						Min: 1,
						Max: 10,
					}
				})

				Context("scale down step size is set", func() {
					BeforeEach(func() {
						testResource.Spec.ScaleConfig.ScaledownStepSize = intstr.FromInt(1000)
					})

					It("should set compute type and processing unit configuration automatically", func() {
						result, err := createResource(testResource)
						Expect(err).ToNot(HaveOccurred())
						Expect(result.Spec.ScaleConfig.ComputeType).To(Equal(spannerv1beta1.ComputeTypeNode))
						Expect(result.Spec.ScaleConfig.ProcessingUnits.Min).To(Equal(1000))
						Expect(result.Spec.ScaleConfig.ProcessingUnits.Max).To(Equal(10000))
						Expect(result.Spec.ScaleConfig.ScaledownStepSize.IntVal).To(Equal(int32(1000)))
					})
				})

				Context("scale down step size is not set", func() {
					It("should set compute type, processing unit configuration, and scale down step size automatically", func() {
						result, err := createResource(testResource)
						Expect(err).ToNot(HaveOccurred())
						Expect(result.Spec.ScaleConfig.ComputeType).To(Equal(spannerv1beta1.ComputeTypeNode))
						Expect(result.Spec.ScaleConfig.ProcessingUnits.Min).To(Equal(1000))
						Expect(result.Spec.ScaleConfig.ProcessingUnits.Max).To(Equal(10000))
						Expect(result.Spec.ScaleConfig.ScaledownStepSize.IntVal).To(Equal(int32(2000)))
					})
				})
			})
		})
	})

	Describe("Check validation settings", func() {
		Describe("Authentication", func() {
			BeforeEach(func() {
				testResource.Spec.ScaleConfig.ProcessingUnits = spannerv1beta1.ScaleConfigPUs{
					Min: 1000,
					Max: 10000,
				}
			})

			Context("both ImpersonateConfig and IAMKeySecret are set", func() {
				BeforeEach(func() {
					testResource.Spec.Authentication = spannerv1beta1.Authentication{
						ImpersonateConfig: &spannerv1beta1.ImpersonateConfig{
							TargetServiceAccount: "test-service-account",
							Delegates:            []string{"test-delegate"},
						},
						IAMKeySecret: &spannerv1beta1.IAMKeySecret{
							Name: "test-service-account-secret",
							Key:  "secret",
						},
					}
				})

				It("should return validation error", func() {
					_, err := createResource(testResource)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("'impersonateConfig' and 'iamKeySecret' are mutually exclusive values"))
				})
			})
		})

		Describe("TargetCPUUtilization", func() {
			BeforeEach(func() {
				testResource.Spec.ScaleConfig.ProcessingUnits = spannerv1beta1.ScaleConfigPUs{
					Min: 1000,
					Max: 10000,
				}
			})

			Context("only total is set", func() {
				BeforeEach(func() {
					testResource.Spec.ScaleConfig.TargetCPUUtilization = spannerv1beta1.TargetCPUUtilization{
						Total: intPtr(60),
					}
				})

				It("should succeed (total-only mode)", func() {
					_, err := createResource(testResource)
					Expect(err).ToNot(HaveOccurred())
				})
			})

			Context("both highPriority and total are set", func() {
				BeforeEach(func() {
					testResource.Spec.ScaleConfig.TargetCPUUtilization = spannerv1beta1.TargetCPUUtilization{
						HighPriority: intPtr(30),
						Total:        intPtr(60),
					}
				})

				It("should succeed (dual CPU scaling mode)", func() {
					_, err := createResource(testResource)
					Expect(err).ToNot(HaveOccurred())
				})
			})

			Context("neither highPriority nor total is set", func() {
				BeforeEach(func() {
					testResource.Spec.ScaleConfig.TargetCPUUtilization = spannerv1beta1.TargetCPUUtilization{}
				})

				It("should return validation error", func() {
					_, err := createResource(testResource)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("at least one of 'highPriority' or 'total' must be specified"))
				})
			})
		})

		Describe("ScaleConfig", func() {
			BeforeEach(func() {
				testResource.Spec.Authentication = spannerv1beta1.Authentication{
					ImpersonateConfig: &spannerv1beta1.ImpersonateConfig{
						TargetServiceAccount: "test-service-account",
						Delegates:            []string{"test-delegate"},
					},
				}
			})

			Context("max nodes are smaller than min nodes", func() {
				BeforeEach(func() {
					testResource.Spec.ScaleConfig.Nodes = spannerv1beta1.ScaleConfigNodes{
						Max: 1,
						Min: 2,
					}
				})

				It("should return validation error", func() {
					_, err := createResource(testResource)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("'max' value must be less than 'min' value"))
				})
			})

			Context("max processing units are smaller than min processing units", func() {
				BeforeEach(func() {
					testResource.Spec.ScaleConfig.ProcessingUnits = spannerv1beta1.ScaleConfigPUs{
						Max: 1000,
						Min: 2000,
					}
				})

				It("should return validation error", func() {
					_, err := createResource(testResource)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("'max' value must be less than 'min' value"))
				})
			})

			Context("processing units are greater than 1000 but not multiples of 1000", func() {
				BeforeEach(func() {
					testResource.Spec.ScaleConfig.ProcessingUnits = spannerv1beta1.ScaleConfigPUs{
						Max: 2000,
						Min: 1500,
					}
				})

				It("should return validation error", func() {
					_, err := createResource(testResource)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("processing units which are greater than 1000, should be multiples of 1000"))
				})
			})
		})

		Describe("TimeBasedOverrides", func() {
			BeforeEach(func() {
				testResource.Spec.ScaleConfig.ProcessingUnits = spannerv1beta1.ScaleConfigPUs{Min: 12000, Max: 80000}
				testResource.Spec.ScaleConfig.TimeBasedOverrides = []spannerv1beta1.TimeBasedOverride{
					{
						Name:              "night",
						Times:             []string{"CRON_TZ=Asia/Tokyo * 1-6 * * *"},
						ProcessingUnits:   &spannerv1beta1.TimeBasedOverridePUs{Min: new(8000)},
						ScaledownStepSize: new(intstr.FromString("30%")),
					},
					{
						Name:            "others",
						Times:           []string{"* * * * *"},
						ScaledownPolicy: spannerv1beta1.ScaledownPolicyDeny,
					},
				}
			})

			It("should keep the overrides without filling defaults into them", func() {
				got, err := createResource(testResource)
				Expect(err).ToNot(HaveOccurred())
				Expect(got.Spec.ScaleConfig.TimeBasedOverrides).To(Equal(testResource.Spec.ScaleConfig.TimeBasedOverrides))
			})

			Context("scaledownAllowedTimes is also set", func() {
				BeforeEach(func() {
					testResource.Spec.ScaleConfig.ScaledownAllowedTimes = []string{"CRON_TZ=Asia/Tokyo * 1-7 * * *"}
				})

				It("should return validation error", func() {
					_, err := createResource(testResource)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("cannot be used together with scaledownAllowedTimes or scaledownNotAllowedTimes"))
				})
			})

			Context("more overrides than the default limit", func() {
				BeforeEach(func() {
					testResource.Spec.ScaleConfig.TimeBasedOverrides = nil
					for i := range DefaultMaxTimeBasedOverrides + 1 {
						testResource.Spec.ScaleConfig.TimeBasedOverrides = append(testResource.Spec.ScaleConfig.TimeBasedOverrides,
							spannerv1beta1.TimeBasedOverride{
								Name:            fmt.Sprintf("o%d", i),
								Times:           []string{"* * * * *"},
								ScaledownPolicy: spannerv1beta1.ScaledownPolicyDeny,
							})
					}
				})

				It("should return validation error", func() {
					_, err := createResource(testResource)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("must have at most 10 items"))
				})
			})

			Context("scaledownPolicy is not in the enum", func() {
				BeforeEach(func() {
					testResource.Spec.ScaleConfig.TimeBasedOverrides[1].ScaledownPolicy = "Sometimes"
				})

				It("should return validation error", func() {
					_, err := createResource(testResource)
					Expect(err).To(HaveOccurred())
					Expect(err.Error()).To(ContainSubstring("scaledownPolicy"))
				})
			})
		})
	})
})

func intPtr(i int) *int { return &i }

func createResource(r *spannerv1beta1.SpannerAutoscaler) (finalResource *spannerv1beta1.SpannerAutoscaler, err error) {
	finalResource = &spannerv1beta1.SpannerAutoscaler{}
	if err := k8sClient.Create(ctx, r); err != nil {
		return finalResource, err
	}

	nn := types.NamespacedName{
		Namespace: r.Namespace,
		Name:      r.Name,
	}
	if err := k8sClient.Get(ctx, nn, finalResource); err != nil {
		return finalResource, err
	}
	return finalResource, nil
}
