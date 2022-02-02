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
				Authentication: Authentication{
					IAMKeySecret: &IAMKeySecret{
						Name:      "test-service-account-secret",
						Namespace: "",
						Key:       "secret",
					},
				},
				ScaleConfig: ScaleConfig{
					ProcessingUnits: ScaleConfigPUs{
						Min: 1000,
						Max: 10000,
					},
					ScaledownStepSize: 2000,
					TargetCPUUtilization: TargetCPUUtilization{
						HighPriority: 30,
					},
				},
			},
		}
	})

	Context("Check default settings", func() {
		It("should set authentication type 'gcp-sa-key' automatically", func() {
			result, err := createResource(testResource)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Spec.Authentication.Type).To(Equal(AuthTypeSA))
		})

		It("should set authentication type 'impersonation' automatically", func() {
			testResource.Spec.Authentication.IAMKeySecret = nil // unset the value set above
			testResource.Spec.Authentication.ImpersonateConfig = &ImpersonateConfig{
				TargetServiceAccount: "dummy@example.com",
			}

			result, err := createResource(testResource)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Spec.Authentication.Type).To(Equal(AuthTypeImpersonation))
		})

		It("should set authentication type 'adc' automatically", func() {
			testResource.Spec.Authentication.IAMKeySecret = nil // unset the value set above

			result, err := createResource(testResource)
			Expect(err).ToNot(HaveOccurred())
			Expect(result.Spec.Authentication.Type).To(Equal(AuthTypeADC))
		})

		// TODO: add more unit tests for verifying all the defaults
		// TODO: add more unit tests for verifying all the validation conditions
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
