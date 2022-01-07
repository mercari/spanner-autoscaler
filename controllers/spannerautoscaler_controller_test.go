package controllers

import (
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/clock"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/go-logr/zapr"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/google/uuid"
	spannerv1alpha1 "github.com/mercari/spanner-autoscaler/api/v1alpha1"
	"github.com/mercari/spanner-autoscaler/pkg/pointer"
	"github.com/mercari/spanner-autoscaler/pkg/syncer"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("SpannerAutoscaler controller", func() {
	var (
		baseObj *spannerv1alpha1.SpannerAutoscaler
	)

	BeforeEach(func() {

		baseObj = &spannerv1alpha1.SpannerAutoscaler{
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
				ServiceAccountSecretRef: &spannerv1alpha1.ServiceAccountSecretRef{
					Name:      pointer.String("test-service-account-secret"),
					Namespace: pointer.String(""),
					Key:       pointer.String("secret"),
				},
			},
			Status: spannerv1alpha1.SpannerAutoscalerStatus{},
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

			targetResource := baseObj.DeepCopy()
			targetResource.Spec.MinNodes = pointer.Int32(1)
			targetResource.Spec.MaxNodes = pointer.Int32(10)
			targetResource.Spec.MaxScaleDownNodes = pointer.Int32(2)
			targetResource.Spec.TargetCPUUtilization = spannerv1alpha1.TargetCPUUtilization{
				HighPriority: pointer.Int32(30),
			}
			targetResource.Status.CurrentNodes = pointer.Int32(3)
			targetResource.Status.InstanceState = spannerv1alpha1.InstanceStateReady
			targetResource.Status.LastScaleTime = &metav1.Time{Time: fakeTime.Add(-2 * time.Hour)} // more than scaleDownInterval
			targetResource.Status.CurrentHighPriorityCPUUtilization = pointer.Int32(50)

			By("Creating a test secret")
			err := k8sClient.Create(ctx, testSecret)
			Expect(err).NotTo(HaveOccurred())

			By("Creating a test SpannerAutoscaler resource")
			err = k8sClient.Create(ctx, targetResource)
			Expect(err).NotTo(HaveOccurred())

			By("Updating the status of SpannerAutoscaler resource")
			err = k8sClient.Status().Update(ctx, targetResource)
			Expect(err).NotTo(HaveOccurred())

			By("Pausing for status changes to propagate")
			time.Sleep(1 * time.Second)

			By("Reconciling the SpannerAutoscaler resource")
			res, err := spReconciler.Reconcile(ctx, ctrlreconcile.Request{
				NamespacedName: namespacedName,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.Requeue).To(BeFalse())

			want := baseObj.DeepCopy()
			want.Status.DesiredNodes = pointer.Int32(6)
			want.Status.DesiredProcessingUnits = pointer.Int32(6000)
			want.Status.InstanceState = spannerv1alpha1.InstanceStateReady
			want.Status.LastScaleTime = &metav1.Time{Time: fakeTime}

			var got spannerv1alpha1.SpannerAutoscaler

			By("Fetching the SpannerAutoscaler resource")
			err = k8sClient.Get(ctx, namespacedName, &got)
			Expect(err).NotTo(HaveOccurred())

			By("Verifying the status of the fetched SpannerAutoscaler resource")
			diff := cmp.Diff(want.Status, got.Status,
				// Ignore CurrentNodes because syncer.Syncer updates it.
				cmpopts.IgnoreFields(want.Status, "CurrentNodes"),
				// Ignore CurrentHighPriorityCPUUtilization because controller doesn't update it.
				cmpopts.IgnoreFields(want.Status, "CurrentHighPriorityCPUUtilization"),
			)
			Expect(diff).To(BeEmpty())
		})
	})
})

var _ = Describe("Check Processing Units", func() {
	var testReconciler *SpannerAutoscalerReconciler

	BeforeEach(func() {
		By("Creating a test reconciler")
		testReconciler = &SpannerAutoscalerReconciler{
			scaleDownInterval: time.Hour,
			clock:             clock.NewFakeClock(fakeTime),
			log:               zapr.NewLogger(zap.NewNop()),
		}
		Expect(testReconciler).NotTo(BeNil())
	})

	It("needs to calculate processing units", func() {
		sa := &spannerv1alpha1.SpannerAutoscaler{
			Status: spannerv1alpha1.SpannerAutoscalerStatus{
				LastScaleTime: &metav1.Time{Time: fakeTime.Add(-2 * time.Hour)},
				CurrentNodes:  pointer.Int32(1),
				InstanceState: spannerv1alpha1.InstanceStateReady,
			},
		}
		Expect(testReconciler.needCalcProcessingUnits(sa)).To(BeTrue())
	})

	It("does not need to calculate processing units because current noddes have not been fetched yet", func() {
		sa := &spannerv1alpha1.SpannerAutoscaler{}
		Expect(testReconciler.needCalcProcessingUnits(sa)).To(BeFalse())
	})

	It("does not need to calculate processing units because instance state is not readdy yet", func() {
		sa := &spannerv1alpha1.SpannerAutoscaler{
			Status: spannerv1alpha1.SpannerAutoscalerStatus{
				CurrentNodes:  pointer.Int32(1),
				InstanceState: spannerv1alpha1.InstanceStateCreating,
			},
		}
		Expect(testReconciler.needCalcProcessingUnits(sa)).To(BeFalse())
	})

})

var _ = Describe("Check Upddate Nodes", func() {
	var testReconciler *SpannerAutoscalerReconciler

	BeforeEach(func() {
		By("Creating a test reconciler")
		testReconciler = &SpannerAutoscalerReconciler{
			scaleDownInterval: time.Hour,
			clock:             clock.NewFakeClock(fakeTime),
			log:               zapr.NewLogger(zap.NewNop()),
		}
	})

	It("does not need to scale down nodes because enough time has not elapsed since last update", func() {
		sa := &spannerv1alpha1.SpannerAutoscaler{
			Status: spannerv1alpha1.SpannerAutoscalerStatus{
				LastScaleTime: &metav1.Time{Time: fakeTime.Add(-time.Minute)},
				CurrentNodes:  pointer.Int32(2),
				DesiredNodes:  pointer.Int32(1),
				InstanceState: spannerv1alpha1.InstanceStateReady,
			},
		}
		got := testReconciler.needUpdateProcessingUnits(sa, normalizeProcessingUnitsOrNodes(sa.Status.DesiredProcessingUnits, sa.Status.DesiredNodes), fakeTime)
		Expect(got).To(BeFalse())
	})

	It("needs to scale up nodes because cooldown interval is only applied to scale down operations", func() {
		sa := &spannerv1alpha1.SpannerAutoscaler{
			Status: spannerv1alpha1.SpannerAutoscalerStatus{
				LastScaleTime: &metav1.Time{Time: fakeTime.Add(-time.Minute)},
				CurrentNodes:  pointer.Int32(1),
				DesiredNodes:  pointer.Int32(2),
				InstanceState: spannerv1alpha1.InstanceStateReady,
			},
		}
		got := testReconciler.needUpdateProcessingUnits(sa, normalizeProcessingUnitsOrNodes(sa.Status.DesiredProcessingUnits, sa.Status.DesiredNodes), fakeTime)
		Expect(got).To(BeTrue())
	})
})

var _ = DescribeTable("Calculate Desired Nodes",
	func(currentCPU, currentNodes, targetCPU, minNodes, maxNodes, maxScaleDownNodes, want int) {
		got := calcDesiredNodes(
			int32(currentCPU),
			int32(currentNodes),
			int32(targetCPU),
			int32(minNodes),
			int32(maxNodes),
			int32(maxScaleDownNodes),
		)
		Expect(got).To(Equal(int32(want)))
	},
	Entry("should not scale", 25, 2, 30, 1, 10, 2, 2),
	Entry("should scale up", 50, 3, 30, 1, 10, 2, 6),
	Entry("should scale down", 30, 5, 50, 1, 10, 2, 4),
	Entry("should scale up to max nodes", 50, 3, 30, 1, 4, 2, 4),
	Entry("should scale down to min nodes", 30, 5, 50, 5, 10, 2, 5),
	Entry("should scale down with MaxScaleDownNodes", 30, 10, 50, 5, 10, 2, 8),
)

var _ = DescribeTable("Calculate Desired Processing Units",
	func(currentCPU, currentProcessingUnits, targetCPU, minProcessingUnits, maxProcessingUnits, maxScaleDownNodes, want int) {
		got := calcDesiredProcessingUnits(
			int32(currentCPU),
			int32(currentProcessingUnits),
			int32(targetCPU),
			int32(minProcessingUnits),
			int32(maxProcessingUnits),
			int32(maxScaleDownNodes),
		)
		Expect(got).To(Equal(int32(want)))
	},
	Entry("should not scale", 25, 200, 30, 100, 1000, 2, 200),
	Entry("should scale up", 50, 300, 30, 100, 1000, 2, 600),
	Entry("should scale up 2", 50, 3000, 30, 1000, 10000, 2, 6000),
	Entry("should scale up 3", 50, 900, 40, 100, 5000, 2, 2000),
	Entry("should scale down", 30, 500, 50, 100, 10000, 2, 400),
	Entry("should scale down 2", 30, 5000, 50, 1000, 10000, 2, 4000),
	Entry("should scale up to max PUs", 50, 300, 30, 100, 400, 2, 400),
	Entry("should scale up to max PUs 2", 50, 3000, 30, 1000, 4000, 2, 4000),
	Entry("should scale down to min PUs", 0, 500, 50, 100, 1000, 2, 100),
	Entry("should scale down to min PUs 2", 0, 5000, 50, 1000, 10000, 5, 1000),
	Entry("should scale down to min PUs 3", 0, 5000, 50, 100, 10000, 5, 100),
	Entry("should scale down with MaxScaleDownNodes", 30, 10000, 50, 5000, 10000, 2, 8000),
)

var _ = DescribeTable("Next Valid ProcessingUnits",
	func(input, want int) {
		got := nextValidProcessingUnits(int32(input))
		Expect(got).To(Equal(int32(want)))
	},
	EntryDescription("Next valid ProcessingUnit for %d is %d"),
	Entry(nil, 0, 100),
	Entry(nil, 99, 100),
	Entry(nil, 100, 200),
	Entry(nil, 900, 1000),
	Entry(nil, 1000, 2000),
	Entry(nil, 1999, 2000),
	Entry(nil, 2000, 3000),
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
		testResource     *spannerv1alpha1.SpannerAutoscaler
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

		testResource = &spannerv1alpha1.SpannerAutoscaler{
			ObjectMeta: metav1.ObjectMeta{
				Name:      testResourceName,
				Namespace: namespace,
			},
			Spec: spannerv1alpha1.SpannerAutoscalerSpec{
				ServiceAccountSecretRef: &spannerv1alpha1.ServiceAccountSecretRef{
					Namespace: pointer.String(namespace),
					Name:      pointer.String(testSecretName),
					Key:       pointer.String(testSecretRefKey),
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

	It("should fetch json correctly even when ServiceAccountSecretRef does not have a namespace", func() {
		testResource.Spec.ServiceAccountSecretRef.Namespace = nil
		result.want = syncer.NewServiceAccountJSONCredentials([]byte(testSecretRefVal))
	})

	It("should return error when no secret key is provided", func() {
		result.expectedErr = errFetchServiceAccountJSONNoKeySpecified
		testResource.Spec.ServiceAccountSecretRef.Key = nil
	})

	It("should return error when no secret data is found in ServiceAccountSecretRef.Key", func() {
		result.expectedErr = errFetchServiceAccountJSONNoSecretDataFound
		testResource.Spec.ServiceAccountSecretRef.Key = pointer.String("invalid-key")
	})

	It("should return error when no secret name is specified", func() {
		result.expectedErr = errFetchServiceAccountJSONNoNameSpecified
		testResource.Spec.ServiceAccountSecretRef.Name = nil
	})

	It("should return error when secret is not found", func() {
		result.expectedErr = errFetchServiceAccountJSONNoSecretFound
		testResource.Spec.ServiceAccountSecretRef.Name = pointer.String("invalid-secret")
	})

	It("should return ADC credentials when ServiceAccountSecretRef is not specified", func() {
		testResource.Spec.ServiceAccountSecretRef = nil
		result.want = syncer.NewADCCredentials()
	})

	It("should return error when both of ServiceAccountSecretRef and InstanceConfig", func() {
		// testResource.Spec.ServiceAccountSecretRef is already set in the default initialization above
		testResource.Spec.ImpersonateConfig = &spannerv1alpha1.ImpersonateConfig{
			TargetServiceAccount: "target@example.iam.gserviceaccount.com",
		}
		result.expectedErr = errInvalidExclusiveCredentials
	})

	It("should return impersonate config when only InstanceConfig is specified", func() {
		// remove the default value which is already set in the initialization above
		testResource.Spec.ServiceAccountSecretRef = nil

		targetSA := "target@example.iam.gserviceaccount.com"
		testResource.Spec.ImpersonateConfig = &spannerv1alpha1.ImpersonateConfig{
			TargetServiceAccount: targetSA,
		}
		result.want = syncer.NewServiceAccountImpersonate(targetSA, nil)
	})
})
