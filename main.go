/*

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"os"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmanager "sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlsignals "sigs.k8s.io/controller-runtime/pkg/manager/signals"

	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	// +kubebuilder:scaffold:imports

	spannerv1alpha1 "github.com/mercari/spanner-autoscaler/api/v1alpha1"
	"github.com/mercari/spanner-autoscaler/controllers"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrllog.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(spannerv1alpha1.AddToScheme(scheme))

	utilruntime.Must(spannerv1beta1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

var (
	metricsAddr          = flag.String("metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	probeAddr            = flag.String("health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	enableLeaderElection = flag.Bool("leader-elect", false, "Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
	scaleDownInterval    = flag.Duration("scale-down-interval", 55*time.Minute, "The scale down interval.")
)

const (
	exitCode         = 1
	leaderElectionID = "spanner-autoscaler-leader-election"
	healthzEndpoint  = "/healthz"
	readyzEndpoint   = "/readyz"
	healthzName      = "healthz"
	readyzName       = "readyz"
)

func main() {
	zapOptions := zap.Options{
		DestWriter: os.Stdout, // default is os.Stderr
	}

	zapOptions.BindFlags(flag.CommandLine)

	flag.Parse()
	log := zap.New(zap.UseFlagOptions(&zapOptions))

	ctrllog.SetLogger(log)

	log.V(1).Info(
		"flags",
		"metricsAddr", metricsAddr,
		"probeAddr", probeAddr,
		"scaleDownInterval", scaleDownInterval,
	)

	cfg, err := config.GetConfig()
	if err != nil {
		setupLog.Error(err, "failed to get config")
		os.Exit(exitCode)
	}

	mgr, err := ctrlmanager.New(cfg, ctrlmanager.Options{
		Scheme:                 scheme,
		LeaderElection:         *enableLeaderElection,
		LeaderElectionID:       leaderElectionID,
		MetricsBindAddress:     *metricsAddr,
		HealthProbeBindAddress: *probeAddr,
		ReadinessEndpointName:  readyzEndpoint,
		LivenessEndpointName:   healthzEndpoint,

		// TODO: remove this when `v1beta1` is stable and tested
		// Only for development
		// CertDir: "./bin/dummytls",
	})
	if err != nil {
		setupLog.Error(err, "failed to create manager")
		os.Exit(exitCode)
	}

	if err := mgr.AddHealthzCheck(healthzName, healthz.Ping); err != nil {
		setupLog.Error(err, "failed to register healthz checker")
		os.Exit(exitCode)
	}

	if err := mgr.AddReadyzCheck(readyzName, healthz.Ping); err != nil {
		setupLog.Error(err, "failed to register readyz checker")
		os.Exit(exitCode)
	}

	sar := controllers.NewSpannerAutoscalerReconciler(
		mgr.GetClient(),
		mgr.GetAPIReader(),
		mgr.GetScheme(),
		mgr.GetEventRecorderFor("spannerautoscaler-controller"),
		log,
		controllers.WithLog(log),
		controllers.WithScaleDownInterval(*scaleDownInterval),
	)
	if err := sar.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "SpannerAutoscaler")
		os.Exit(exitCode)
	}

	if err = (&spannerv1beta1.SpannerAutoscaler{}).SetupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "SpannerAutoscaler")
		os.Exit(exitCode)
	}

	sasr := controllers.NewSpannerAutoscaleScheduleReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		controllers.WithLog(log),
	)

	if err = sasr.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "SpannerAutoscaleSchedule")
		os.Exit(exitCode)
	}
	//+kubebuilder:scaffold:builder

	setupLog.Info("starting manager")

	if err := mgr.Start(ctrlsignals.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(exitCode)
	}
}
