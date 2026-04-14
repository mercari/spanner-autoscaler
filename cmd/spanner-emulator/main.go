package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	instancepb "cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc"

	"github.com/mercari/spanner-autoscaler/internal/spanneremulator"
)

func main() {
	grpcPort := envOrDefault("GRPC_PORT", "9010")
	adminPort := envOrDefault("ADMIN_PORT", "9011")

	logger := slog.Default()

	srv := spanneremulator.NewServer()

	// Start gRPC server.
	grpcLis, err := net.Listen("tcp", ":"+grpcPort)
	if err != nil {
		logger.Error("failed to listen for gRPC", "port", grpcPort, "error", err)
		os.Exit(1)
	}
	grpcSrv := grpc.NewServer()
	instancepb.RegisterInstanceAdminServer(grpcSrv, srv)

	go func() {
		logger.Info("gRPC server listening", "port", grpcPort)
		if err := grpcSrv.Serve(grpcLis); err != nil {
			logger.Error("gRPC server error", "error", err)
		}
	}()

	// Start HTTP admin server.
	adminSrv := &http.Server{ //nolint:gosec
		Addr:    ":" + adminPort,
		Handler: spanneremulator.NewAdminHandler(srv),
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
