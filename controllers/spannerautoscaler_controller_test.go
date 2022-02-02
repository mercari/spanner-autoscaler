package controllers

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/clock"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/uuid"
	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	"github.com/mercari/spanner-autoscaler/internal/syncer"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("SpannerAutoscaler controller", func() {
	var baseObj *spannerv1beta1.SpannerAutoscaler

	BeforeEach(func() {
		baseObj = &spannerv1beta1.SpannerAutoscaler{
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
				Authentication: spannerv1beta1.Authentication{
					Type: spannerv1beta1.AuthTypeSA,
					IAMKeySecret: &spannerv1beta1.IAMKeySecret{
						Name:      "test-service-account-secret",
						Namespace: "",
						Key:       "secret",
					},
				},
				ScaleConfig: spannerv1beta1.ScaleConfig{
					ComputeType: spannerv1beta1.ComputeTypePU,
					ProcessingUnits: spannerv1beta1.ScaleConfigPUs{
						Min: 1000,
						Max: 10000,
					},
					ScaledownStepSize: 2000,
					TargetCPUUtilization: spannerv1beta1.TargetCPUUtilization{
						HighPriority: 30,
					},
				},
			},
		}
	})

	Context("Check Reconciler", func() {
		It("should scale up spanner instance nodes", func() {
			testSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-service-account-secret",
					Namespace: namespace,
				},
				StringData: map[string]string{"secret": `{"foo":"bar"}`},
			}

			targetResource := baseObj

			By("Creating a test secret")
			err := k8sClient.Create(ctx, testSecret)
			Expect(err).NotTo(HaveOccurred())

			By("Creating a test SpannerAutoscaler resource")
			err = k8sClient.Create(ctx, targetResource)
			Expect(err).NotTo(HaveOccurred())

			By("Fetching the created SpannerAutoscaler resource")
			err = k8sClient.Get(ctx, namespacedName, targetResource)
			Expect(err).NotTo(HaveOccurred())

			By("Updating the status of SpannerAutoscaler resource")
			initialStatus := spannerv1beta1.SpannerAutoscalerStatus{
				CurrentProcessingUnits:            3000,
				InstanceState:                     spannerv1beta1.InstanceStateReady,
				LastScaleTime:                     metav1.Time{Time: fakeTime.Add(-2 * time.Hour)}, // more than scaleDownInterval
				CurrentHighPriorityCPUUtilization: 50,
			}
			targetResource.Status = initialStatus
			err = k8sClient.Status().Update(ctx, targetResource)
			Expect(err).NotTo(HaveOccurred())

			By("Pausing for reconciliation to complete")
			time.Sleep(1 * time.Second)

			By("Fetching the SpannerAutoscaler resource")
			got := &spannerv1beta1.SpannerAutoscaler{}
			err = k8sClient.Get(ctx, namespacedName, got)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the status of the fetched SpannerAutoscaler resource")
			wantStatus := spannerv1beta1.SpannerAutoscalerStatus{
				DesiredNodes:           6,
				DesiredProcessingUnits: 6000,
				InstanceState:          spannerv1beta1.InstanceStateReady,
				LastScaleTime:          metav1.Time{Time: fakeTime},
			}
			diff := cmp.Diff(wantStatus, got.Status,
				// Ignore CurrentProcessingUnits because syncer.Syncer updates it.
				cmpopts.IgnoreFields(wantStatus, "CurrentProcessingUnits"),
				// Ignore CurrentHighPriorityCPUUtilization because controller doesn't update it.
				cmpopts.IgnoreFields(wantStatus, "CurrentHighPriorityCPUUtilization"),
			)
			Expect(diff).To(BeEmpty())
		})
	})
})

var _ = Describe("Check Update Nodes", func() {
	var testReconciler *SpannerAutoscalerReconciler

	BeforeEach(func() {
		By("Creating a test reconciler")
		testReconciler = &SpannerAutoscalerReconciler{
			scaleDownInterval: time.Hour,
			clock:             clock.NewFakeClock(fakeTime),
			log:               logr.Discard(),
		}
	})

	It("does not need to scale down nodes because enough time has not elapsed since last update", func() {
		sa := &spannerv1beta1.SpannerAutoscaler{
			Status: spannerv1beta1.SpannerAutoscalerStatus{
				LastScaleTime:          metav1.Time{Time: fakeTime.Add(-time.Minute)},
				CurrentProcessingUnits: 2000,
				DesiredProcessingUnits: 1000,
				InstanceState:          spannerv1beta1.InstanceStateReady,
			},
		}
		got := testReconciler.needUpdateProcessingUnits(testReconciler.log, sa, sa.Status.DesiredProcessingUnits, fakeTime)
		Expect(got).To(BeFalse())
	})

	It("needs to scale up nodes because cooldown interval is only applied to scale down operations", func() {
		sa := &spannerv1beta1.SpannerAutoscaler{
			Spec: spannerv1beta1.SpannerAutoscalerSpec{
				ScaleConfig: spannerv1beta1.ScaleConfig{
					ComputeType: spannerv1beta1.ComputeTypeNode,
				},
			},
			Status: spannerv1beta1.SpannerAutoscalerStatus{
				LastScaleTime:          metav1.Time{Time: fakeTime.Add(-time.Minute)},
				CurrentProcessingUnits: 1000,
				DesiredProcessingUnits: 2000,
				InstanceState:          spannerv1beta1.InstanceStateReady,
			},
		}
		got := testReconciler.needUpdateProcessingUnits(testReconciler.log, sa, sa.Status.DesiredProcessingUnits, fakeTime)
		Expect(got).To(BeTrue())
	})
})

var _ = DescribeTable("Calculate Desired Processing Units",
	func(currentCPU, currentProcessingUnits, targetCPU, minProcessingUnits, maxProcessingUnits, scaledownStepSize, want int) {
		baseObj := spannerv1beta1.SpannerAutoscaler{}
		baseObj.Status.CurrentProcessingUnits = currentProcessingUnits
		baseObj.Status.CurrentHighPriorityCPUUtilization = currentCPU
		baseObj.Spec.ScaleConfig = spannerv1beta1.ScaleConfig{
			ComputeType: spannerv1beta1.ComputeTypePU,
			ProcessingUnits: spannerv1beta1.ScaleConfigPUs{
				Min: minProcessingUnits,
				Max: maxProcessingUnits,
			},
			ScaledownStepSize: scaledownStepSize,
			TargetCPUUtilization: spannerv1beta1.TargetCPUUtilization{
				HighPriority: targetCPU,
			},
		}
		got := calcDesiredProcessingUnits(baseObj)
		Expect(got).To(Equal(want))
	},
	Entry("should not scale", 25, 200, 30, 100, 1000, 2000, 200),
	Entry("should scale up", 50, 300, 30, 100, 1000, 2000, 600),
	Entry("should scale up 2", 50, 3000, 30, 1000, 10000, 2000, 6000),
	Entry("should scale up 3", 50, 900, 40, 100, 5000, 2000, 2000),
	Entry("should scale down", 30, 500, 50, 100, 10000, 2000, 400),
	Entry("should scale down 2", 30, 5000, 50, 1000, 10000, 2000, 4000),
	Entry("should scale up to max PUs", 50, 300, 30, 100, 400, 2000, 400),
	Entry("should scale up to max PUs 2", 50, 3000, 30, 1000, 4000, 2000, 4000),
	Entry("should scale down to min PUs", 0, 500, 50, 100, 1000, 2000, 100),
	Entry("should scale down to min PUs 2", 0, 5000, 50, 1000, 10000, 5000, 1000),
	Entry("should scale down to min PUs 3", 0, 5000, 50, 100, 10000, 5000, 100),
	Entry("should scale down with MaxScaleDownNodes", 30, 10000, 50, 5000, 10000, 2000, 8000),
)

var _ = Describe("Fetch Credentials", func() {
	type testResult struct {
		want        *syncer.Credentials
		expectedErr error
	}

	var (
		result           testResult
		testSecret       *corev1.Secret
		testSecretRefKey string
		testSecretRefVal string
		random           string
		testResource     *spannerv1beta1.SpannerAutoscaler
		testReconciler   *SpannerAutoscalerReconciler
	)

	BeforeEach(func() {
		random = uuid.NewString()
		testSecretRefKey = "service-account"
		testSecretRefVal = `{"foo":"bar"}`
		result = testResult{}

		testSecretName := "secret-" + random
		testResourceName := "spanner-autoscaler-" + random

		testSecret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testSecretName,
				Namespace: namespace,
			},
			StringData: map[string]string{testSecretRefKey: testSecretRefVal},
		}

		testResource = &spannerv1beta1.SpannerAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testResourceName,
				Namespace: namespace,
			},
			Spec: spannerv1beta1.SpannerAutoscalerSpec{
				Authentication: spannerv1beta1.Authentication{
					Type: spannerv1beta1.AuthTypeSA,
					IAMKeySecret: &spannerv1beta1.IAMKeySecret{
						Namespace: namespace,
						Name:      testSecretName,
						Key:       testSecretRefKey,
					},
				},
			},
		}

		testReconciler = &SpannerAutoscalerReconciler{
			ctrlClient: k8sClient,
			apiReader:  k8sClient,
		}
	})

	AfterEach(func() {
		if testSecret != nil {
			By("Creating a Secret")
			err := k8sClient.Create(ctx, testSecret)
			Expect(err).ToNot(HaveOccurred())
		}

		By("Fetching credentials")
		got, err := testReconciler.fetchCredentials(ctx, testResource)

		if result.expectedErr == nil && err != nil {
			By("Failed to fetch credentials")
			Expect(err).ToNot(HaveOccurred())
		}

		if result.expectedErr != nil {
			By("Comparing expected and received errors")
			Expect(err).To(MatchError(result.expectedErr))
		}

		By("Comparing the values")
		// using 'cmp.Diff' over 'Expect(want).To(Equal(got))' makes it
		// easier to spot the difference when tests fail
		Expect(cmp.Diff(result.want, got)).To(BeEmpty())
	})

	It("should fetch json correctly", func() {
		result.want = syncer.NewServiceAccountJSONCredentials([]byte(testSecretRefVal))
	})

	It("should fetch correctly even when non-json data is provided in the secret", func() {
		testSecret.StringData = map[string]string{testSecretRefKey: "non{json-data"}
		result.want = syncer.NewServiceAccountJSONCredentials([]byte("non{json-data"))
	})

	It("should fetch json correctly even when IAMKeySecret does not have a namespace", func() {
		testResource.Spec.Authentication.IAMKeySecret.Namespace = ""
		result.want = syncer.NewServiceAccountJSONCredentials([]byte(testSecretRefVal))
	})

	It("should return error when no secret key is provided", func() {
		result.expectedErr = errFetchServiceAccountJSONNoKeySpecified
		testResource.Spec.Authentication.IAMKeySecret.Key = ""
	})

	It("should return error when no secret data is found in IAMKeySecret.Key", func() {
		result.expectedErr = errFetchServiceAccountJSONNoSecretDataFound
		testResource.Spec.Authentication.IAMKeySecret.Key = "invalid-key"
	})

	It("should return error when no secret name is specified", func() {
		result.expectedErr = errFetchServiceAccountJSONNoNameSpecified
		testResource.Spec.Authentication.IAMKeySecret.Name = ""
	})

	It("should return error when secret is not found", func() {
		result.expectedErr = errFetchServiceAccountJSONNoSecretFound
		testResource.Spec.Authentication.IAMKeySecret.Name = "invalid-secret"
	})

	It("should return ADC credentials when IAMKeySecret is not specified", func() {
		testResource.Spec.Authentication.Type = spannerv1beta1.AuthTypeADC
		testResource.Spec.Authentication.IAMKeySecret = nil
		result.want = syncer.NewADCCredentials()
	})

	It("should return error when both of IAMKeySecret and InstanceConfig", func() {
		// testResource.Spec.Authentication.IAMKeySecret is already set in the default initialization above
		testResource.Spec.Authentication.ImpersonateConfig = &spannerv1beta1.ImpersonateConfig{
			TargetServiceAccount: "target@example.iam.gserviceaccount.com",
		}
		result.expectedErr = errInvalidExclusiveCredentials
	})

	It("should return impersonate config when only InstanceConfig is specified", func() {
		// remove the default value which is already set in the initialization above
		testResource.Spec.Authentication.Type = spannerv1beta1.AuthTypeImpersonation
		testResource.Spec.Authentication.IAMKeySecret = nil

		targetSA := "target@example.iam.gserviceaccount.com"
		testResource.Spec.Authentication.ImpersonateConfig = &spannerv1beta1.ImpersonateConfig{
			TargetServiceAccount: targetSA,
		}
		result.want = syncer.NewServiceAccountImpersonate(targetSA, nil)
	})
})
