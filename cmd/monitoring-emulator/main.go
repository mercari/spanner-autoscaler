package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	monitoringpb "cloud.google.com/go/monitoring/apiv3/v2/monitoringpb"
	spanneradmin "cloud.google.com/go/spanner/admin/instance/apiv1"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/mercari/spanner-autoscaler/internal/monitoringemulator"
)

func main() {
	grpcPort := envOrDefault("GRPC_PORT", "9090")
	adminPort := envOrDefault("ADMIN_PORT", "9091")
	spannerEmulatorHost := os.Getenv("SPANNER_EMULATOR_HOST")
	scenarioFile := os.Getenv("SCENARIO_FILE")

	logger := slog.Default()

	staticStore := monitoringemulator.NewStaticStore()
	workloadStore := monitoringemulator.NewWorkloadStore()
	scenarioStore := monitoringemulator.NewScenarioStore()

	if scenarioFile != "" {
		if err := scenarioStore.LoadFile(scenarioFile); err != nil {
			logger.Error("failed to load scenario file", "path", scenarioFile, "error", err)
			os.Exit(1)
		}
		logger.Info("scenario file loaded", "path", scenarioFile)
	}

	// Create Spanner Admin client for dynamic (workload) mode.
	// Only available when SPANNER_EMULATOR_HOST is set.
	var spannerAdminClient *spanneradmin.InstanceAdminClient
	if spannerEmulatorHost != "" {
		logger.Info("dynamic mode enabled", "spanner_emulator_host", spannerEmulatorHost)
		var err error
		spannerAdminClient, err = spanneradmin.NewInstanceAdminClient(
			context.Background(),
			option.WithEndpoint(spannerEmulatorHost),
			option.WithoutAuthentication(),
			option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
		)
		if err != nil {
			logger.Error("failed to create spanner admin client", "error", err)
			os.Exit(1)
		}
		defer spannerAdminClient.Close() //nolint:errcheck
	} else {
		logger.Info("dynamic mode disabled: SPANNER_EMULATOR_HOST not set")
	}

	srv := monitoringemulator.NewMetricServiceServer(staticStore, workloadStore, scenarioStore, spannerAdminClient)

	// Start gRPC server.
	grpcLis, err := net.Listen("tcp", ":"+grpcPort)
	if err != nil {
		logger.Error("failed to listen for gRPC", "port", grpcPort, "error", err)
		os.Exit(1) //nolint:gocritic
	}
	grpcSrv := grpc.NewServer()
	monitoringpb.RegisterMetricServiceServer(grpcSrv, srv)

	go func() {
		logger.Info("gRPC server listening", "port", grpcPort)
		if err := grpcSrv.Serve(grpcLis); err != nil {
			logger.Error("gRPC server error", "error", err)
		}
	}()

	// Start HTTP admin server.
	adminHandler := monitoringemulator.NewAdminHandler(staticStore, workloadStore, scenarioStore)
	adminSrv := &http.Server{ //nolint:gosec
		Addr:    ":" + adminPort,
		Handler: adminHandler,
	}
	go func() {
		logger.Info("admin HTTP server listening", "port", adminPort)
		if err := adminSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("admin HTTP server error", "error", err)
		}
	}()

	// Wait for SIGINT or SIGTERM.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	sig := <-quit
	logger.Info("shutting down", "signal", sig)

	grpcSrv.GracefulStop()
	if err := adminSrv.Shutdown(context.Background()); err != nil {
		logger.Error("admin server shutdown error", "error", err)
	}
}

func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
