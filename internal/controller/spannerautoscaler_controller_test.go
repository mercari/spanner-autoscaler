package controller

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	testingclock "k8s.io/utils/clock/testing"

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
					ScaledownStepSize: intstr.FromInt(2000),
					TargetCPUUtilization: spannerv1beta1.TargetCPUUtilization{
						HighPriority: intPtr(30),
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
				CurrentCPUMetricType:              spannerv1beta1.CPUMetricTypeHighPriority,
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
				DesiredProcessingUnits: 6000,
				DesiredMinPUs:          targetResource.Spec.ScaleConfig.ProcessingUnits.Min,
				DesiredMaxPUs:          targetResource.Spec.ScaleConfig.ProcessingUnits.Max,
				InstanceState:          spannerv1beta1.InstanceStateReady,
				LastScaleTime:          metav1.Time{Time: fakeTime},
			}
			diff := cmp.Diff(wantStatus, got.Status,
				// Ignore CurrentProcessingUnits because syncer.Syncer updates it.
				cmpopts.IgnoreFields(wantStatus, "CurrentProcessingUnits"),
				// Ignore CurrentHighPriorityCPUUtilization because controller doesn't update it.
				cmpopts.IgnoreFields(wantStatus, "CurrentHighPriorityCPUUtilization"),
				// Ignore CurrentCPUMetricType because only syncer.Syncer sets it, not the controller.
				cmpopts.IgnoreFields(wantStatus, "CurrentCPUMetricType"),
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
			scaleUpInterval:   time.Hour,
			clock:             testingclock.NewFakeClock(fakeTime),
			log:               logr.Discard(),
		}
	})

	It("need to scale down nodes because enough time has elapsed since last update", func() {
		sa := &spannerv1beta1.SpannerAutoscaler{
			Status: spannerv1beta1.SpannerAutoscalerStatus{
				LastScaleTime:          metav1.Time{Time: fakeTime.Add(-2 * time.Hour)},
				CurrentProcessingUnits: 2000,
				DesiredProcessingUnits: 1000,
				InstanceState:          spannerv1beta1.InstanceStateReady,
			},
		}
		got := testReconciler.needUpdateProcessingUnits(testReconciler.log, sa, sa.Status.DesiredProcessingUnits, fakeTime)
		Expect(got).To(BeTrue())
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

	It("need to scale up nodes because enough time has elapsed since last update", func() {
		sa := &spannerv1beta1.SpannerAutoscaler{
			Spec: spannerv1beta1.SpannerAutoscalerSpec{
				ScaleConfig: spannerv1beta1.ScaleConfig{
					ComputeType: spannerv1beta1.ComputeTypeNode,
				},
			},
			Status: spannerv1beta1.SpannerAutoscalerStatus{
				LastScaleTime:          metav1.Time{Time: fakeTime.Add(-2 * time.Hour)},
				CurrentProcessingUnits: 1000,
				DesiredProcessingUnits: 2000,
				InstanceState:          spannerv1beta1.InstanceStateReady,
			},
		}
		got := testReconciler.needUpdateProcessingUnits(testReconciler.log, sa, sa.Status.DesiredProcessingUnits, fakeTime)
		Expect(got).To(BeTrue())
	})

	It("does not need to scale up nodes because enough time has not elapsed since last update", func() {
		sa := &spannerv1beta1.SpannerAutoscaler{
			Status: spannerv1beta1.SpannerAutoscalerStatus{
				LastScaleTime:          metav1.Time{Time: fakeTime.Add(-time.Minute)},
				CurrentProcessingUnits: 1000,
				DesiredProcessingUnits: 2000,
				InstanceState:          spannerv1beta1.InstanceStateReady,
			},
		}
		got := testReconciler.needUpdateProcessingUnits(testReconciler.log, sa, sa.Status.DesiredProcessingUnits, fakeTime)
		Expect(got).To(BeFalse())
	})
})

var _ = DescribeTable("Calculate Desired Processing Units",
	func(currentCPU, currentProcessingUnits, targetCPU, minProcessingUnits, maxProcessingUnits int, scaledownStepSize, scaleupStepSize intstr.IntOrString, want int) {
		baseObj := spannerv1beta1.SpannerAutoscaler{}
		baseObj.Status.CurrentProcessingUnits = currentProcessingUnits
		baseObj.Status.CurrentHighPriorityCPUUtilization = currentCPU
		baseObj.Status.CurrentCPUMetricType = spannerv1beta1.CPUMetricTypeHighPriority
		baseObj.Spec.ScaleConfig = spannerv1beta1.ScaleConfig{
			ComputeType: spannerv1beta1.ComputeTypePU,
			ProcessingUnits: spannerv1beta1.ScaleConfigPUs{
				Min: minProcessingUnits,
				Max: maxProcessingUnits,
			},
			ScaledownStepSize: scaledownStepSize,
			ScaleupStepSize:   scaleupStepSize,
			TargetCPUUtilization: spannerv1beta1.TargetCPUUtilization{
				HighPriority: intPtr(targetCPU),
			},
		}
		got := calcDesiredProcessingUnits(baseObj)
		Expect(got).To(Equal(want))
	},
	Entry("should not scale", 25, 200, 30, 100, 1000, intstr.FromInt(2000), intstr.FromInt(0), 200),
	Entry("should scale up 1", 50, 300, 30, 100, 1000, intstr.FromInt(2000), intstr.FromInt(0), 600),
	Entry("should scale up 2", 50, 1000, 30, 1000, 10000, intstr.FromInt(2000), intstr.FromInt(0), 2000),
	Entry("should scale up 3", 50, 900, 40, 100, 5000, intstr.FromInt(2000), intstr.FromInt(0), 2000),
	Entry("should scale down 1", 30, 500, 50, 100, 10000, intstr.FromInt(2000), intstr.FromInt(0), 400),
	Entry("should scale down 2", 30, 5000, 50, 1000, 10000, intstr.FromInt(2000), intstr.FromInt(0), 4000),
	Entry("should scale down 3", 25, 1000, 65, 300, 10000, intstr.FromInt(800), intstr.FromInt(0), 400),
	Entry("should scale down 4", 25, 800, 65, 300, 10000, intstr.FromInt(800), intstr.FromInt(0), 400),
	Entry("should scale down 5", 25, 700, 65, 300, 10000, intstr.FromInt(800), intstr.FromInt(0), 300),
	Entry("should scale up to max PUs 1", 50, 300, 30, 100, 400, intstr.FromInt(2000), intstr.FromInt(0), 400),
	Entry("should scale up to max PUs 2", 50, 3000, 30, 1000, 4000, intstr.FromInt(2000), intstr.FromInt(0), 4000),
	Entry("should scale down to min PUs 1", 0, 500, 50, 100, 1000, intstr.FromInt(2000), intstr.FromInt(0), 100),
	Entry("should scale down to min PUs 2", 0, 5000, 50, 1000, 10000, intstr.FromInt(5000), intstr.FromInt(0), 1000),
	Entry("should scale down to min PUs 3", 0, 5000, 50, 100, 10000, intstr.FromInt(5000), intstr.FromInt(0), 100),
	Entry("should scale down with ScaledownStepSize 1", 30, 10000, 50, 5000, 10000, intstr.FromInt(2000), intstr.FromInt(0), 8000),
	Entry("should scale down with ScaledownStepSize 2", 30, 10000, 50, 5000, 12000, intstr.FromInt(200), intstr.FromInt(0), 9000),
	Entry("should scale down with ScaledownStepSize 3", 30, 10000, 50, 5000, 12000, intstr.FromInt(100), intstr.FromInt(0), 9000),
	Entry("should scale down with ScaledownStepSize 4", 30, 10000, 50, 5000, 12000, intstr.FromInt(900), intstr.FromInt(0), 9000),
	Entry("should scale down with ScaledownStepSize 5", 25, 2000, 65, 300, 10000, intstr.FromInt(500), intstr.FromInt(0), 1000),
	Entry("should scale down with ScaledownStepSize 6", 25, 2000, 65, 300, 10000, intstr.FromInt(800), intstr.FromInt(0), 1000),
	Entry("should scale down with ScaledownStepSize 7", 25, 1000, 65, 300, 10000, intstr.FromInt(500), intstr.FromInt(0), 500),
	Entry("should scale down with ScaledownStepSize 8", 20, 800, 75, 300, 10000, intstr.FromInt(200), intstr.FromInt(0), 600),
	Entry("should scale down with ScaledownStepSize 9", 25, 2000, 50, 300, 10000, intstr.FromInt(500), intstr.FromInt(0), 2000),
	Entry("should scale down with ScaledownStepSize 10", 25, 2000, 50, 300, 10000, intstr.FromInt(1000), intstr.FromInt(0), 2000),
	Entry("should scale down with ScaledownStepSize 11", 25, 2000, 70, 300, 10000, intstr.FromInt(400), intstr.FromInt(0), 1000),
	Entry("should scale down with ScaledownStepSize 12", 25, 1000, 70, 300, 10000, intstr.FromInt(400), intstr.FromInt(0), 600),
	Entry("should scale down with ScaledownStepSize 13", 20, 2000, 50, 300, 10000, intstr.FromInt(200), intstr.FromInt(0), 1000),
	Entry("should scale down with ScaledownStepSize 14", 20, 2000, 75, 300, 10000, intstr.FromInt(200), intstr.FromInt(0), 1000),
	Entry("should scale down with ScaledownStepSize 2%", 30, 1000, 50, 300, 10000, intstr.FromString("2%"), intstr.FromInt(0), 900),
	Entry("should scale down with ScaledownStepSize 5%", 30, 10000, 50, 5000, 10000, intstr.FromString("5%"), intstr.FromInt(0), 9000),
	Entry("should scale down with ScaledownStepSize 10%", 30, 10000, 50, 5000, 10000, intstr.FromString("10%"), intstr.FromInt(0), 9000),
	Entry("should scale down with ScaledownStepSize 15%", 30, 10000, 50, 5000, 10000, intstr.FromString("15%"), intstr.FromInt(0), 9000),
	Entry("should scale down with ScaledownStepSize 20%", 30, 10000, 50, 5000, 10000, intstr.FromString("20%"), intstr.FromInt(0), 8000),
	Entry("should scale down with ScaledownStepSize 25%", 30, 10000, 50, 5000, 10000, intstr.FromString("25%"), intstr.FromInt(0), 8000),
	Entry("should scale down with ScaledownStepSize 30%", 30, 10000, 50, 5000, 10000, intstr.FromString("30%"), intstr.FromInt(0), 7000),
	Entry("should scale down with ScaledownStepSize 40%", 30, 10000, 50, 5000, 10000, intstr.FromString("40%"), intstr.FromInt(0), 7000),
	Entry("should scale down with ScaledownStepSize 50%", 30, 10000, 50, 5000, 10000, intstr.FromString("50%"), intstr.FromInt(0), 7000),
	Entry("should scale down with ScaledownStepSize 60%", 30, 1000, 50, 300, 10000, intstr.FromString("60%"), intstr.FromInt(0), 700),
	Entry("should scale up with ScaleupStepSize when currentPU is equal to zero 1", 80, 100, 10, 100, 10000, intstr.FromInt(2000), intstr.FromInt(0), 900),
	Entry("should scale up with ScaleupStepSize when currentPU is equal to zero 2", 100, 100, 10, 100, 10000, intstr.FromInt(2000), intstr.FromInt(0), 2000),
	Entry("should scale up with ScaleupStepSize when currentPU is lower than 1000 1", 80, 100, 10, 100, 10000, intstr.FromInt(2000), intstr.FromInt(700), 800),
	Entry("should scale up with ScaleupStepSize when currentPU is lower than 1000 2", 100, 300, 10, 100, 10000, intstr.FromInt(2000), intstr.FromInt(700), 1000),
	Entry("should scale up with ScaleupStepSize when currentPU is lower than 1000 3", 100, 800, 10, 100, 10000, intstr.FromInt(2000), intstr.FromInt(700), 2000),
	Entry("should scale up with ScaleupStepSize when currentPU is lower than 1000 4", 80, 100, 10, 100, 10000, intstr.FromInt(2000), intstr.FromString("200%"), 300),
	Entry("should scale up with ScaleupStepSize when currentPU is lower than 1000 5", 100, 300, 10, 100, 10000, intstr.FromInt(2000), intstr.FromString("200%"), 900),
	Entry("should scale up with ScaleupStepSize when currentPU is lower than 1000 6", 100, 800, 10, 100, 10000, intstr.FromInt(2000), intstr.FromString("200%"), 2000),
	Entry("should scale up with ScaleupStepSize when currentPU is equal to 1000 1", 100, 1000, 50, 100, 10000, intstr.FromInt(2000), intstr.FromInt(700), 2000),
	Entry("should scale up with ScaleupStepSize when currentPU is equal to 1000 2", 100, 1000, 30, 100, 10000, intstr.FromInt(2000), intstr.FromInt(2000), 3000),
	Entry("should scale up with ScaleupStepSize when currentPU is equal to 1000 3", 100, 1000, 10, 100, 10000, intstr.FromInt(2000), intstr.FromInt(3000), 4000),
	Entry("should scale up with ScaleupStepSize when currentPU is equal to 1000 4", 100, 1000, 50, 100, 10000, intstr.FromInt(2000), intstr.FromString("70%"), 2000),
	Entry("should scale up with ScaleupStepSize when currentPU is equal to 1000 5", 100, 1000, 30, 100, 10000, intstr.FromInt(2000), intstr.FromString("200%"), 3000),
	Entry("should scale up with ScaleupStepSize when currentPU is equal to 1000 6", 100, 1000, 10, 100, 10000, intstr.FromInt(2000), intstr.FromString("300%"), 4000),
	Entry("should scale up with ScaleupStepSize when currentPU is more than 1000 1", 30, 2000, 20, 100, 10000, intstr.FromInt(2000), intstr.FromInt(700), 3000),
	Entry("should scale up with ScaleupStepSize when currentPU is more than 1000 2", 20, 2000, 10, 100, 10000, intstr.FromInt(2000), intstr.FromInt(2000), 4000),
	Entry("should scale up with ScaleupStepSize when currentPU is more than 1000 3", 100, 2000, 10, 100, 10000, intstr.FromInt(2000), intstr.FromInt(3000), 5000),
	Entry("should scale up with ScaleupStepSize when currentPU is more than 1000 4", 30, 2000, 20, 100, 10000, intstr.FromInt(2000), intstr.FromString("70%"), 3000),
	Entry("should scale up with ScaleupStepSize when currentPU is more than 1000 5", 20, 2000, 10, 100, 10000, intstr.FromInt(2000), intstr.FromString("100%"), 4000),
	Entry("should scale up with ScaleupStepSize when currentPU is more than 1000 6", 100, 2000, 10, 100, 10000, intstr.FromInt(2000), intstr.FromString("200%"), 6000),
)

var _ = Describe("Calculate Desired Processing Units (metric type switching guard)", func() {
	baseScaleConfig := func() spannerv1beta1.ScaleConfig {
		return spannerv1beta1.ScaleConfig{
			ComputeType: spannerv1beta1.ComputeTypePU,
			ProcessingUnits: spannerv1beta1.ScaleConfigPUs{
				Min: 100,
				Max: 10000,
			},
			ScaledownStepSize: intstr.FromInt(2000),
			ScaleupStepSize:   intstr.FromInt(0),
		}
	}

	It("keeps current PU when switching highPriority→dual before first sync (CurrentCPUMetricType still HighPriority)", func() {
		sa := spannerv1beta1.SpannerAutoscaler{}
		sa.Status.CurrentProcessingUnits = 3000
		sa.Status.CurrentCPUMetricType = spannerv1beta1.CPUMetricTypeHighPriority // syncer hasn't run yet for dual mode
		sa.Status.CurrentHighPriorityCPUUtilization = 80
		sa.Status.CurrentTotalCPUUtilization = 0
		sa.Spec.ScaleConfig = baseScaleConfig()
		sa.Spec.ScaleConfig.TargetCPUUtilization = spannerv1beta1.TargetCPUUtilization{
			HighPriority: intPtr(60), // spec switched to dual mode
			Total:        intPtr(40),
		}
		Expect(calcDesiredProcessingUnits(sa)).To(Equal(3000))
	})

	It("keeps current PU when switching total→highPriority before first sync (CurrentCPUMetricType still Total)", func() {
		sa := spannerv1beta1.SpannerAutoscaler{}
		sa.Status.CurrentProcessingUnits = 3000
		sa.Status.CurrentCPUMetricType = spannerv1beta1.CPUMetricTypeTotal // syncer hasn't run yet
		sa.Status.CurrentTotalCPUUtilization = 26
		sa.Status.CurrentHighPriorityCPUUtilization = 0
		sa.Spec.ScaleConfig = baseScaleConfig()
		sa.Spec.ScaleConfig.TargetCPUUtilization = spannerv1beta1.TargetCPUUtilization{
			HighPriority: intPtr(40), // spec switched to highPriority
		}
		Expect(calcDesiredProcessingUnits(sa)).To(Equal(3000))
	})

	It("scales normally after first sync following total→highPriority switch", func() {
		sa := spannerv1beta1.SpannerAutoscaler{}
		sa.Status.CurrentProcessingUnits = 3000
		sa.Status.CurrentCPUMetricType = spannerv1beta1.CPUMetricTypeHighPriority // syncer ran and updated
		sa.Status.CurrentTotalCPUUtilization = 0
		sa.Status.CurrentHighPriorityCPUUtilization = 20
		sa.Spec.ScaleConfig = baseScaleConfig()
		sa.Spec.ScaleConfig.TargetCPUUtilization = spannerv1beta1.TargetCPUUtilization{
			HighPriority: intPtr(40),
		}
		// 20/40 * 3000 = 1500 → rounds up to 2000
		Expect(calcDesiredProcessingUnits(sa)).To(Equal(2000))
	})

	It("keeps current PU for newly created resource before first sync (CurrentCPUMetricType empty)", func() {
		sa := spannerv1beta1.SpannerAutoscaler{}
		sa.Status.CurrentProcessingUnits = 1000
		// CurrentCPUMetricType is "" (zero value) — syncer has not run yet
		sa.Spec.ScaleConfig = baseScaleConfig()
		sa.Spec.ScaleConfig.TargetCPUUtilization = spannerv1beta1.TargetCPUUtilization{
			HighPriority: intPtr(40),
		}
		Expect(calcDesiredProcessingUnits(sa)).To(Equal(1000))
	})

	It("keeps current PU when neither highPriority nor total is set (invalid spec)", func() {
		sa := spannerv1beta1.SpannerAutoscaler{}
		sa.Status.CurrentProcessingUnits = 3000
		sa.Status.CurrentCPUMetricType = spannerv1beta1.CPUMetricTypeHighPriority
		sa.Spec.ScaleConfig = baseScaleConfig()
		// Both HighPriority and Total are nil — invalid spec that bypassed webhook validation
		sa.Spec.ScaleConfig.TargetCPUUtilization = spannerv1beta1.TargetCPUUtilization{}
		Expect(calcDesiredProcessingUnits(sa)).To(Equal(3000))
	})

	// Dual CPU scaling mode tests
	It("dual mode: keeps current PU until syncer populates Both status (guard)", func() {
		sa := spannerv1beta1.SpannerAutoscaler{}
		sa.Status.CurrentProcessingUnits = 3000
		sa.Status.CurrentCPUMetricType = spannerv1beta1.CPUMetricTypeHighPriority // syncer hasn't run dual yet
		sa.Status.CurrentHighPriorityCPUUtilization = 80
		sa.Status.CurrentTotalCPUUtilization = 0
		sa.Spec.ScaleConfig = baseScaleConfig()
		sa.Spec.ScaleConfig.TargetCPUUtilization = spannerv1beta1.TargetCPUUtilization{
			HighPriority: intPtr(60),
			Total:        intPtr(40),
		}
		Expect(calcDesiredProcessingUnits(sa)).To(Equal(3000))
	})

	It("dual mode: scales up when highPriority exceeds threshold", func() {
		sa := spannerv1beta1.SpannerAutoscaler{}
		sa.Status.CurrentProcessingUnits = 3000
		sa.Status.CurrentCPUMetricType = spannerv1beta1.CPUMetricTypeBoth
		sa.Status.CurrentHighPriorityCPUUtilization = 90 // exceeds target 60
		sa.Status.CurrentTotalCPUUtilization = 20        // below target 40
		sa.Spec.ScaleConfig = baseScaleConfig()
		sa.Spec.ScaleConfig.TargetCPUUtilization = spannerv1beta1.TargetCPUUtilization{
			HighPriority: intPtr(60),
			Total:        intPtr(40),
		}
		// highPriority: 90/60 * 3000 = 4500 → 5000; total: 20/40 * 3000 = 1500 → 2000 → max = 5000
		Expect(calcDesiredProcessingUnits(sa)).To(Equal(5000))
	})

	It("dual mode: scales up when total exceeds threshold", func() {
		sa := spannerv1beta1.SpannerAutoscaler{}
		sa.Status.CurrentProcessingUnits = 3000
		sa.Status.CurrentCPUMetricType = spannerv1beta1.CPUMetricTypeBoth
		sa.Status.CurrentHighPriorityCPUUtilization = 30 // below target 60
		sa.Status.CurrentTotalCPUUtilization = 60        // exceeds target 40
		sa.Spec.ScaleConfig = baseScaleConfig()
		sa.Spec.ScaleConfig.TargetCPUUtilization = spannerv1beta1.TargetCPUUtilization{
			HighPriority: intPtr(60),
			Total:        intPtr(40),
		}
		// highPriority: 30/60 * 3000 = 1500 → 2000; total: 60/40 * 3000 = 4500 → 5000 → max = 5000
		Expect(calcDesiredProcessingUnits(sa)).To(Equal(5000))
	})

	It("dual mode: uses max of both when both exceed thresholds", func() {
		sa := spannerv1beta1.SpannerAutoscaler{}
		sa.Status.CurrentProcessingUnits = 3000
		sa.Status.CurrentCPUMetricType = spannerv1beta1.CPUMetricTypeBoth
		sa.Status.CurrentHighPriorityCPUUtilization = 80 // exceeds target 60
		sa.Status.CurrentTotalCPUUtilization = 70        // exceeds target 40
		sa.Spec.ScaleConfig = baseScaleConfig()
		sa.Spec.ScaleConfig.TargetCPUUtilization = spannerv1beta1.TargetCPUUtilization{
			HighPriority: intPtr(60),
			Total:        intPtr(40),
		}
		// highPriority: 80/60 * 3000 = 4000 → 5000; total: 70/40 * 3000 = 5250 → 6000 → max = 6000
		Expect(calcDesiredProcessingUnits(sa)).To(Equal(6000))
	})

	It("dual mode: scales down when neither metric exceeds threshold", func() {
		sa := spannerv1beta1.SpannerAutoscaler{}
		sa.Status.CurrentProcessingUnits = 5000
		sa.Status.CurrentCPUMetricType = spannerv1beta1.CPUMetricTypeBoth
		sa.Status.CurrentHighPriorityCPUUtilization = 20 // below target 60
		sa.Status.CurrentTotalCPUUtilization = 15        // below target 40
		sa.Spec.ScaleConfig = baseScaleConfig()
		sa.Spec.ScaleConfig.TargetCPUUtilization = spannerv1beta1.TargetCPUUtilization{
			HighPriority: intPtr(60),
			Total:        intPtr(40),
		}
		// highPriority: 20/60 * 5000 = 1666 → 1700; total: 15/40 * 5000 = 1875 → 1900 → max = 1900
		// scale down by at most scaledownStepSize=2000: max(1900, 5000-2000=3000) = 3000
		Expect(calcDesiredProcessingUnits(sa)).To(Equal(3000))
	})
})

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

var _ = Describe("Get and overwrite interval", func() {
	defaultInterval := 55 * time.Minute

	It("should get defaultInterval if customInterval == nil", func() {
		want := defaultInterval
		got := getOrConvertTimeDuration(nil, defaultInterval)
		Expect(got).To(Equal(want))
	})

	It("should override defaultInterval with customInterval.Duration if customInterval != nil", func() {
		want := 20 * time.Minute
		customInterval := &metav1.Duration{Duration: want}
		got := getOrConvertTimeDuration(customInterval, defaultInterval)
		Expect(got).To(Equal(want))
	})
})

var _ = Describe("Calculate Desired PU Range", func() {
	baseSpec := func(min, max int) spannerv1beta1.SpannerAutoscaler {
		sa := spannerv1beta1.SpannerAutoscaler{}
		sa.Spec.ScaleConfig.ProcessingUnits.Min = min
		sa.Spec.ScaleConfig.ProcessingUnits.Max = max
		return sa
	}

	It("returns spec min/max when no active schedules", func() {
		sa := baseSpec(1000, 10000)
		sa.Status.DesiredMinPUs = 1000
		sa.Status.DesiredMaxPUs = 10000
		gotMin, gotMax, changed := calcDesiredPURange(sa)
		Expect(gotMin).To(Equal(1000))
		Expect(gotMax).To(Equal(10000))
		Expect(changed).To(BeFalse())
	})

	It("adds AdditionalPU to spec min/max for a single active schedule", func() {
		sa := baseSpec(1000, 10000)
		sa.Status.CurrentlyActiveSchedules = []spannerv1beta1.ActiveSchedule{
			{AdditionalPU: 2000},
		}
		gotMin, gotMax, changed := calcDesiredPURange(sa)
		Expect(gotMin).To(Equal(3000))
		Expect(gotMax).To(Equal(12000))
		Expect(changed).To(BeTrue())
	})

	It("sums AdditionalPU from all active schedules for both min and max", func() {
		sa := baseSpec(1000, 10000)
		sa.Status.CurrentlyActiveSchedules = []spannerv1beta1.ActiveSchedule{
			{AdditionalPU: 1000},
			{AdditionalPU: 5000},
		}
		gotMin, gotMax, changed := calcDesiredPURange(sa)
		// desiredMin = 1000 + (1000 + 5000) = 7000
		// desiredMax = 10000 + (1000 + 5000) = 16000
		Expect(gotMin).To(Equal(7000))
		Expect(gotMax).To(Equal(16000))
		Expect(changed).To(BeTrue())
	})

	It("rounds up desiredMin and desiredMax to the next 1000 when above 1000 and not a multiple", func() {
		sa := baseSpec(1000, 10000)
		sa.Status.CurrentlyActiveSchedules = []spannerv1beta1.ActiveSchedule{
			{AdditionalPU: 1500},
		}
		gotMin, gotMax, changed := calcDesiredPURange(sa)
		// desiredMin = 1000 + 1500 = 2500 → rounded up to 3000
		// desiredMax = 10000 + 1500 = 11500 → rounded up to 12000
		Expect(gotMin).To(Equal(3000))
		Expect(gotMax).To(Equal(12000))
		Expect(changed).To(BeTrue())
	})

	It("does not round when desiredMin/Max is already a multiple of 1000", func() {
		sa := baseSpec(1000, 10000)
		sa.Status.CurrentlyActiveSchedules = []spannerv1beta1.ActiveSchedule{
			{AdditionalPU: 3000},
		}
		gotMin, gotMax, changed := calcDesiredPURange(sa)
		Expect(gotMin).To(Equal(4000))
		Expect(gotMax).To(Equal(13000))
		Expect(changed).To(BeTrue())
	})

	It("reports no change when computed values match current status", func() {
		sa := baseSpec(1000, 10000)
		sa.Status.CurrentlyActiveSchedules = []spannerv1beta1.ActiveSchedule{
			{AdditionalPU: 2000},
		}
		sa.Status.DesiredMinPUs = 3000
		sa.Status.DesiredMaxPUs = 12000
		_, _, changed := calcDesiredPURange(sa)
		Expect(changed).To(BeFalse())
	})
})

func intPtr(i int) *int { return &i }
