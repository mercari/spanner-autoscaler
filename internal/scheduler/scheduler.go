package scheduler

import (
	"context"
	"time"

	"github.com/go-logr/logr"
	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	cronpkg "github.com/robfig/cron/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

type Scheduler interface {
	Start()
	Stop()
}

type scheduler struct {
	log      logr.Logger
	interval time.Duration
	stopCh   chan struct{}

	ctrlClient ctrlclient.Client

	crons map[types.NamespacedName]*cronpkg.Cron
}

// Job implements cronpkg.Job
type Job struct {
	ScheduleName   types.NamespacedName
	AutoscalerName types.NamespacedName
	Log            logr.Logger
	CtrlClient     ctrlclient.Client
}

func New(log logr.Logger, ctrlClient ctrlclient.Client, crons map[types.NamespacedName]*cronpkg.Cron) scheduler {
	return scheduler{
		log:        log.WithName("scheduler"),
		interval:   10 * time.Second,
		stopCh:     make(chan struct{}),
		ctrlClient: ctrlClient,
		crons:      crons,
	}
}

// Start implements Scheduler
func (s scheduler) Start() {
	log := s.log

	log.V(1).Info("starting scheduler")
	defer log.V(1).Info("shutting down scheduler")

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			log.V(1).Info("scheduler tick received")
			s.UpdateAll()
		case <-s.stopCh:
			log.V(1).Info("received stop signal")
			return
		}
	}
}

// Stop implements Scheduler
func (s scheduler) Stop() {
	close(s.stopCh)
}

// TODO: refactor this package to run one scheduler per SpannerAutoscaler resource, instead of the current approach of one shceduler for updating all SpannerAutoscaler resources.

func (s scheduler) UpdateAll() {
	allSchedules := &spannerv1beta1.SpannerAutoscaleScheduleList{}
	allAutoscalers := &spannerv1beta1.SpannerAutoscalerList{}

	ctx := context.Background()

	if err := s.ctrlClient.List(ctx, allSchedules); err != nil {
		s.log.Error(err, "could not fetch list of spanner-schedule resources")
	}

	if err := s.ctrlClient.List(ctx, allAutoscalers); err != nil {
		s.log.Error(err, "could not fetch list of spanner-autoscaler resources")
	}

	for i := range allAutoscalers.Items {
		s.Update(ctx, &allAutoscalers.Items[i])
	}
}

func (s scheduler) Update(ctx context.Context, autoscaler *spannerv1beta1.SpannerAutoscaler) {
	log := s.log.WithValues("autoscaler", ctrlclient.ObjectKeyFromObject(autoscaler))

	log.V(1).Info("confirming autoscaler schedules", "autoscaler schedules", autoscaler.Status.Schedules, "autoscaler active schedules", autoscaler.Status.CurrentlyActiveSchedules)
	statusChanged := false

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := s.ctrlClient.Get(ctx, ctrlclient.ObjectKeyFromObject(autoscaler), autoscaler); err != nil {
			return err
		}

		autoscaler.Status.CurrentlyActiveSchedules, statusChanged = cleanupActiveSchedules(autoscaler.Status.CurrentlyActiveSchedules)

		if statusChanged {
			return s.ctrlClient.Status().Update(ctx, autoscaler)
		}
		return nil
	})
	if err != nil {
		// May be conflict if max retries were hit, or may be something unrelated
		// like permissions or a network error
		log.Error(err, "failed to update spanner-autoscaler status")
	}
}

func cleanupActiveSchedules(activeSchedules []spannerv1beta1.ActiveSchedule) ([]spannerv1beta1.ActiveSchedule, bool) {
	changed := false
	result := []spannerv1beta1.ActiveSchedule{}
	now := metav1.Now()
	for _, as := range activeSchedules {
		if as.EndTime.Before(&now) {
			changed = true
			continue
		}
		result = append(result, as)
	}
	return result, changed
}

func (j Job) Run() {
	j.Log.V(1).Info("Cron Job is now run", "now", metav1.Now())
	var (
		sa  spannerv1beta1.SpannerAutoscaler
		sas spannerv1beta1.SpannerAutoscaleSchedule
	)

	ctx := context.Background()

	if err := j.CtrlClient.Get(ctx, j.ScheduleName, &sas); err != nil {
		j.Log.Error(err, "failed to get spanner-autoscale-schedule")
	}
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := j.CtrlClient.Get(ctx, j.AutoscalerName, &sa); err != nil {
			j.Log.Error(err, "failed to get spanner-autoscaler")
		}

		duration, err := time.ParseDuration(sas.Spec.Schedule.Duration)
		if err != nil {
			j.Log.Error(err, "failed to parse duration from schedule")
		}

		activeSchedules := []spannerv1beta1.ActiveSchedule{}
		for _, as := range sa.Status.CurrentlyActiveSchedules {
			if as.ScheduleName != j.ScheduleName.String() {
				activeSchedules = append(activeSchedules, as)
			}
		}
		cas := spannerv1beta1.ActiveSchedule{
			ScheduleName: j.ScheduleName.String(),
			AdditionalPU: sas.Spec.AdditionalProcessingUnits,
			EndTime:      metav1.Time{Time: metav1.Now().Add(duration)},
		}
		j.Log.V(1).Info("updating active schedules", "now", metav1.Now(), "endtime", metav1.Now().Add(duration))

		activeSchedules = append(activeSchedules, cas)
		sa.Status.CurrentlyActiveSchedules = activeSchedules

		return j.CtrlClient.Status().Update(ctx, &sa)
	})
	if err != nil {
		// May be conflict if max retries were hit, or may be something unrelated
		// like permissions or a network error
		j.Log.Error(err, "failed to update spanner-autoscaler status for CurrentlyActiveSchedules")
	}
	j.Log.V(1).Info("Cron Job run is now over")
}
