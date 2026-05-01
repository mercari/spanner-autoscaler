package v1beta1

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var _ = Describe("SpannerAutoscaleSchedule validation", func() {
	var (
		schedule  *SpannerAutoscaleSchedule
		namespace string
	)

	BeforeEach(func() {
		namespace = "default"
		schedule = &SpannerAutoscaleSchedule{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-schedule-",
				Namespace:    namespace,
			},
			Spec: SpannerAutoscaleScheduleSpec{
				TargetResource: "spannerautoscaler-sample",
				Schedule: Schedule{
					Cron:     "0 9 * * 1-5",
					Duration: "1h",
				},
				AdditionalProcessingUnits: 500,
			},
		}
	})

	Describe("ValidateCreate", func() {
		Context("with a valid cron and duration", func() {
			It("should succeed", func() {
				Expect(k8sClient.Create(ctx, schedule)).To(Succeed())
				k8sClient.Delete(ctx, schedule) //nolint:errcheck
			})
		})

		Context("with an invalid cron expression", func() {
			BeforeEach(func() {
				schedule.Spec.Schedule.Cron = "not-a-cron"
			})

			It("should return a validation error", func() {
				err := k8sClient.Create(ctx, schedule)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("spec.schedule.cron"))
			})
		})

		Context("with an invalid duration", func() {
			BeforeEach(func() {
				schedule.Spec.Schedule.Duration = "not-a-duration"
			})

			It("should return a validation error", func() {
				err := k8sClient.Create(ctx, schedule)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("spec.schedule.duration"))
			})
		})
	})

	Describe("ValidateUpdate", func() {
		var created *SpannerAutoscaleSchedule

		BeforeEach(func() {
			created = schedule.DeepCopy()
			Expect(k8sClient.Create(ctx, created)).To(Succeed())

			var got SpannerAutoscaleSchedule
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Namespace: created.Namespace,
				Name:      created.Name,
			}, &got)).To(Succeed())
			created = &got
		})

		AfterEach(func() {
			k8sClient.Delete(ctx, created) //nolint:errcheck
		})

		Context("when targetResource is changed", func() {
			It("should return a validation error", func() {
				created.Spec.TargetResource = "other-autoscaler"
				err := k8sClient.Update(ctx, created)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("targetResource"))
			})
		})

		Context("when schedule is changed", func() {
			It("should not return a validation error", func() {
				created.Spec.Schedule.Cron = "0 10 * * 1-5"
				err := k8sClient.Update(ctx, created)
				Expect(err).ToNot(HaveOccurred())
			})
		})

		Context("when only additionalProcessingUnits is changed", func() {
			It("should succeed", func() {
				created.Spec.AdditionalProcessingUnits = 1000
				Expect(k8sClient.Update(ctx, created)).To(Succeed())
			})
		})
	})
})
