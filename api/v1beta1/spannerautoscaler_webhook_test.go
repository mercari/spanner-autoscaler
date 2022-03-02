package v1beta1

import (
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe("SpannerAutoscaler validation", func() {
	var (
		testResource *SpannerAutoscaler
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

		testResource = &SpannerAutoscaler{
			TypeMeta: metav1.TypeMeta{
				Kind:       "SpannerAutoscaler",
				APIVersion: "spanner.mercari.com/v1beta1",
			},
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Spec: SpannerAutoscalerSpec{
				TargetInstance: TargetInstance{
					ProjectID:  "test-project-id",
					InstanceID: "test-instance-id",
				},
				ScaleConfig: ScaleConfig{
					TargetCPUUtilization: TargetCPUUtilization{
						HighPriority: 30,
					},
				},
			},
		}
	})

	Describe("Check default settings", func() {
		Describe("Authentication", func() {
			BeforeEach(func() {
				testResource.Spec.ScaleConfig.ProcessingUnits = ScaleConfigPUs{
					Min: 1000,
					Max: 10000,
				}
			})

			Context("IAM key secret is set", func() {
				BeforeEach(func() {
					testResource.Spec.Authentication = Authentication{
						IAMKeySecret: &IAMKeySecret{
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
						Expect(result.Spec.Authentication.Type).To(Equal(AuthTypeSA))
						Expect(result.Spec.Authentication.IAMKeySecret.Namespace).To(Equal("default"))
					})
				})

				Context("namespace does not exist", func() {
					It("should set authentication type 'gcp-sa-key' and namespace automatically", func() {
						result, err := createResource(testResource)
						Expect(err).ToNot(HaveOccurred())
						Expect(result.Spec.Authentication.Type).To(Equal(AuthTypeSA))
						Expect(result.Spec.Authentication.IAMKeySecret.Namespace).To(Equal(namespace))
					})
				})
			})

			Context("impersonate config is set", func() {
				BeforeEach(func() {
					testResource.Spec.Authentication = Authentication{
						ImpersonateConfig: &ImpersonateConfig{
							TargetServiceAccount: "test-service-account",
							Delegates:            []string{"test-delegate"},
						},
					}
				})

				It("should set authentication type 'impersonation' automatically", func() {
					result, err := createResource(testResource)
					Expect(err).ToNot(HaveOccurred())
					Expect(result.Spec.Authentication.Type).To(Equal(AuthTypeImpersonation))
				})
			})

			Context("no additional authentication config is set", func() {
				It("should set authentication type 'adc' automatically", func() {
					result, err := createResource(testResource)
					Expect(err).ToNot(HaveOccurred())
					Expect(result.Spec.Authentication.Type).To(Equal(AuthTypeADC))
				})
			})
		})

		Describe("ScaleConfig", func() {
			Context("processing unit configuration is set", func() {
				BeforeEach(func() {
					testResource.Spec.ScaleConfig.ProcessingUnits = ScaleConfigPUs{
						Min: 1000,
						Max: 10000,
					}
				})

				Context("scale down step size is set", func() {
					BeforeEach(func() {
						testResource.Spec.ScaleConfig.ScaledownStepSize = 1000
					})

					It("should set compute type 'processing-units' automatically", func() {
						result, err := createResource(testResource)
						Expect(err).ToNot(HaveOccurred())
						Expect(result.Spec.ScaleConfig.ComputeType).To(Equal(ComputeTypePU))
						Expect(result.Spec.ScaleConfig.ScaledownStepSize).To(Equal(1000))
					})
				})

				Context("scale down step size is not set", func() {
					It("should set compute type 'processing-units' and scale down step size automatically", func() {
						result, err := createResource(testResource)
						Expect(err).ToNot(HaveOccurred())
						Expect(result.Spec.ScaleConfig.ComputeType).To(Equal(ComputeTypePU))
						Expect(result.Spec.ScaleConfig.ScaledownStepSize).To(Equal(2000))
					})
				})
			})

			Context("processing unit node is set", func() {
				BeforeEach(func() {
					testResource.Spec.ScaleConfig.Nodes = ScaleConfigNodes{
						Min: 1,
						Max: 10,
					}
				})

				Context("scale down step size is set", func() {
					BeforeEach(func() {
						testResource.Spec.ScaleConfig.ScaledownStepSize = 1000
					})

					It("should set compute type and processing unit configuration automatically", func() {
						result, err := createResource(testResource)
						Expect(err).ToNot(HaveOccurred())
						Expect(result.Spec.ScaleConfig.ComputeType).To(Equal(ComputeTypeNode))
						Expect(result.Spec.ScaleConfig.ProcessingUnits.Min).To(Equal(1000))
						Expect(result.Spec.ScaleConfig.ProcessingUnits.Max).To(Equal(10000))
						Expect(result.Spec.ScaleConfig.ScaledownStepSize).To(Equal(1000))
					})
				})

				Context("scale down step size is not set", func() {
					It("should set compute type, processing unit configuration, and scale down step size automatically", func() {
						result, err := createResource(testResource)
						Expect(err).ToNot(HaveOccurred())
						Expect(result.Spec.ScaleConfig.ComputeType).To(Equal(ComputeTypeNode))
						Expect(result.Spec.ScaleConfig.ProcessingUnits.Min).To(Equal(1000))
						Expect(result.Spec.ScaleConfig.ProcessingUnits.Max).To(Equal(10000))
						Expect(result.Spec.ScaleConfig.ScaledownStepSize).To(Equal(2000))
					})
				})
			})
		})
	})

	Describe("Check validation settings", func() {
		Describe("Authentication", func() {
			BeforeEach(func() {
				testResource.Spec.ScaleConfig.ProcessingUnits = ScaleConfigPUs{
					Min: 1000,
					Max: 10000,
				}
			})

			Context("both ImpersonateConfig and IAMKeySecret are set", func() {
				BeforeEach(func() {
					testResource.Spec.Authentication = Authentication{
						ImpersonateConfig: &ImpersonateConfig{
							TargetServiceAccount: "test-service-account",
							Delegates:            []string{"test-delegate"},
						},
						IAMKeySecret: &IAMKeySecret{
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

		Describe("ScaleConfig", func() {
			BeforeEach(func() {
				testResource.Spec.Authentication = Authentication{
					ImpersonateConfig: &ImpersonateConfig{
						TargetServiceAccount: "test-service-account",
						Delegates:            []string{"test-delegate"},
					},
				}
			})

			Context("max nodes are smaller than min nodes", func() {
				BeforeEach(func() {
					testResource.Spec.ScaleConfig.Nodes = ScaleConfigNodes{
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
					testResource.Spec.ScaleConfig.ProcessingUnits = ScaleConfigPUs{
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
					testResource.Spec.ScaleConfig.ProcessingUnits = ScaleConfigPUs{
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
	})
})

func createResource(r *SpannerAutoscaler) (finalResource *SpannerAutoscaler, err error) {
	finalResource = &SpannerAutoscaler{}
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
