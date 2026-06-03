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
	"crypto/tls"
	"flag"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	spannerv1alpha1 "github.com/mercari/spanner-autoscaler/api/v1alpha1"
	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
	"github.com/mercari/spanner-autoscaler/internal/controller"
	"github.com/mercari/spanner-autoscaler/internal/observability"
	webhookv1beta1 "github.com/mercari/spanner-autoscaler/internal/webhook/v1beta1"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(spannerv1alpha1.AddToScheme(scheme))
	utilruntime.Must(spannerv1beta1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

var (
	// Defaults match the values previously shipped in
	// config/manager/controller_manager_config.yaml so deployments that don't
	// pass these flags explicitly keep their existing behavior.
	metricsAddr          = flag.String("metrics-bind-address", ":8080", "The address the metrics endpoint binds to. Use :8443 for HTTPS or :8080 for HTTP, or set as 0 to disable the metrics service.")
	probeAddr            = flag.String("health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	enableLeaderElection = flag.Bool("leader-elect", true, "Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")
	leaderElectionID     = flag.String("leader-elect-id", "54b82eb3.mercari.com", "Lease name for leader election.")
	secureMetrics        = flag.Bool("metrics-secure", true, "If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	enableHTTP2          = flag.Bool("enable-http2", false, "If set, HTTP/2 will be enabled for the metrics and webhook servers")
	webhookCertPath      = flag.String("webhook-cert-path", "", "The directory that contains the webhook certificate.")
	webhookCertName      = flag.String("webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	webhookCertKey       = flag.String("webhook-cert-key", "tls.key", "The name of the webhook key file.")
	metricsCertPath      = flag.String("metrics-cert-path", "", "The directory that contains the metrics server certificate.")
	metricsCertName      = flag.String("metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	metricsCertKey       = flag.String("metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	scaleDownInterval    = flag.Duration("scale-down-interval", 55*time.Minute, "The scale down interval.")
	scaleUpInterval      = flag.Duration("scale-up-interval", 60*time.Second, "The scale up interval.")
	certDir              = flag.String("cert-dir", "", "Directory containing TLS certificates for the webhook server. Used for local development with webhook forwarding (see docs/development.md).")
	spannerEndpoint      = flag.String("spanner-endpoint", "", "Override the Spanner API endpoint (e.g. localhost:9010 for the emulator).")
	metricsEndpoint      = flag.String("metrics-endpoint", "", "Override the Cloud Monitoring API endpoint (e.g. localhost:9090 for the emulator).")
)

// nolint:gocyclo
func main() {
	var tlsOpts []func(*tls.Config)

	zapOptions := zap.Options{
		DestWriter: os.Stdout, // default is os.Stderr
	}
	zapOptions.BindFlags(flag.CommandLine)
	flag.Parse()

	logger := zap.New(zap.UseFlagOptions(&zapOptions))
	ctrl.SetLogger(logger)

	// TODO: iterate over all flags and dump all values not just this limited list
	logger.V(1).Info(
		"flags",
		"metricsAddr", metricsAddr,
		"probeAddr", probeAddr,
		"scaleDownInterval", scaleDownInterval,
		"scaleUpInterval", scaleUpInterval,
	)

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !*enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(*webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", *webhookCertPath, "webhook-cert-name", *webhookCertName, "webhook-cert-key", *webhookCertKey)

		webhookServerOptions.CertDir = *webhookCertPath
		webhookServerOptions.CertName = *webhookCertName
		webhookServerOptions.KeyName = *webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	metricsServerOptions := metricsserver.Options{
		BindAddress:   *metricsAddr,
		SecureServing: *secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if *secureMetrics {
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	if len(*metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", *metricsCertPath, "metrics-cert-name", *metricsCertName, "metrics-cert-key", *metricsCertKey)

		metricsServerOptions.CertDir = *metricsCertPath
		metricsServerOptions.CertName = *metricsCertName
		metricsServerOptions.KeyName = *metricsCertKey
	}

	if err := observability.Register(ctrlmetrics.Registry); err != nil {
		setupLog.Error(err, "failed to register custom metrics")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: *probeAddr,
		LeaderElection:         *enableLeaderElection,
		LeaderElectionID:       *leaderElectionID,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	reconcilerOpts := []controller.Option{
		controller.WithLog(logger),
		controller.WithScaleDownInterval(*scaleDownInterval),
		controller.WithScaleUpInterval(*scaleUpInterval),
	}
	if *spannerEndpoint != "" {
		reconcilerOpts = append(reconcilerOpts, controller.WithSpannerEndpoint(*spannerEndpoint))
	}
	if *metricsEndpoint != "" {
		reconcilerOpts = append(reconcilerOpts, controller.WithMetricsEndpoint(*metricsEndpoint))
	}

	sar := controller.NewSpannerAutoscalerReconciler(
		mgr.GetClient(),
		mgr.GetAPIReader(),
		mgr.GetScheme(),
		mgr.GetEventRecorderFor("spannerautoscaler-controller"), //nolint:staticcheck // Migrating to the new events.EventRecorder API requires a wholesale refactor of event call sites; tracked separately.
		logger,
		reconcilerOpts...,
	)
	if err := sar.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "spannerautoscaler")
		os.Exit(1)
	}

	sasr := controller.NewSpannerAutoscaleScheduleReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		mgr.GetEventRecorderFor("spannerautoscaleschedule-controller"), //nolint:staticcheck // Migrating to the new events.EventRecorder API requires a wholesale refactor of event call sites; tracked separately.
		controller.WithLog(logger),
	)
	if err := sasr.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "spannerautoscaleschedule")
		os.Exit(1)
	}

	// nolint:goconst
	if os.Getenv("ENABLE_WEBHOOKS") != "false" {
		if err := webhookv1beta1.SetupSpannerAutoscalerWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "Failed to create webhook", "webhook", "SpannerAutoscaler")
			os.Exit(1)
		}
		if err := webhookv1beta1.SetupSpannerAutoscaleScheduleWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "Failed to create webhook", "webhook", "SpannerAutoscaleSchedule")
			os.Exit(1)
		}
	} else {
		setupLog.Info("webhooks disabled (ENABLE_WEBHOOKS=false)")
	}
	// +kubebuilder:scaffold:builder

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
