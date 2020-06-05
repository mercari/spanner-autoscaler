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
	"fmt"
	"os"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/healthz"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmanager "sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlsignals "sigs.k8s.io/controller-runtime/pkg/manager/signals"

	// +kubebuilder:scaffold:imports

	spannerv1alpha1 "github.com/mercari/spanner-autoscaler/pkg/api/v1alpha1"
	"github.com/mercari/spanner-autoscaler/pkg/controllers"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrllog.Log.WithName("setup")
)

func init() {
	//nolint:errcheck
	clientgoscheme.AddToScheme(scheme)

	//nolint:errcheck
	spannerv1alpha1.AddToScheme(scheme)

	// +kubebuilder:scaffold:scheme
}

var (
	metricsAddr          = flag.String("metrics-addr", "localhost:8090", "The address the metric endpoint binds to.")
	enableLeaderElection = flag.Bool("enable-leader-election", false, "Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
	healthzAddr          = flag.String("healthz-addr", "localhost:8091", "The address the healthz endpoint binds to.")
	scaleDownInterval    = flag.Duration("scale-down-interval", 55*time.Minute, "The scale down interval.")
)

const (
	exitCode         = 1
	leaderElectionID = "spanner-autoscaler-leader-election"

	healthzEndpoint = "/healthz"
	readyzEndpoint  = "/readyz"
	healthzName     = "healthz"
	readyzName      = "readyz"
)

func main() {
	flag.Parse()

	if err := run(); err != nil {
		setupLog.Error(err, "unable to run controller")
		os.Exit(exitCode)
	}
}

func run() error {
	log := ctrlzap.New(func(o *ctrlzap.Options) {
		o.Development = true
	})
	ctrllog.SetLogger(log)

	log.V(1).Info(
		"flags",
		"metricsAddr", metricsAddr,
		"enableLeaderElection", enableLeaderElection,
	)

	cfg, err := config.GetConfig()
	if err != nil {
		return err
	}

	mgr, err := ctrlmanager.New(cfg, ctrlmanager.Options{
		Scheme:                 scheme,
		LeaderElection:         *enableLeaderElection,
		LeaderElectionID:       leaderElectionID,
		MetricsBindAddress:     *metricsAddr,
		HealthProbeBindAddress: *healthzAddr,
		ReadinessEndpointName:  readyzEndpoint,
		LivenessEndpointName:   healthzEndpoint,
	})
	if err != nil {
		return err
	}

	if err := mgr.AddHealthzCheck(healthzName, healthz.Ping); err != nil {
		return fmt.Errorf("failed to register healthz checker: %w", err)
	}

	if err := mgr.AddReadyzCheck(readyzName, healthz.Ping); err != nil {
		return fmt.Errorf("failed to register readyz checker: %w", err)
	}

	r := controllers.NewSpannerAutoscalerReconciler(
		mgr.GetClient(),
		mgr.GetAPIReader(),
		mgr.GetScheme(),
		mgr.GetEventRecorderFor("spannerautoscaler-controller"),
		controllers.WithLog(log.WithName("controllers")),
		controllers.WithScaleDownInterval(*scaleDownInterval),
	)
	if err = r.SetupWithManager(mgr); err != nil {
		return err
	}
	// +kubebuilder:scaffold:builder

	setupLog.Info("starting manager")

	if err := mgr.Start(ctrlsignals.SetupSignalHandler()); err != nil {
		return err
	}

	return nil
}
