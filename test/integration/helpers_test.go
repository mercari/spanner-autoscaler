//go:build integration

package integration_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	spannerv1alpha1 "github.com/mercari/spanner-autoscaler/api/v1alpha1"
	spannerv1beta1 "github.com/mercari/spanner-autoscaler/api/v1beta1"
)

func spannerEmulatorAddr() string {
	if v := os.Getenv("SPANNER_EMULATOR_HOST"); v != "" {
		return v
	}
	return "localhost:9010"
}

func spannerAdminAddr() string {
	if v := os.Getenv("SPANNER_EMULATOR_ADMIN_ADDR"); v != "" {
		return v
	}
	return "localhost:9011"
}

func monitoringGRPCAddr() string {
	if v := os.Getenv("MONITORING_EMULATOR_GRPC_ADDR"); v != "" {
		return v
	}
	return "localhost:9090"
}

func monitoringAdminAddr() string {
	if v := os.Getenv("MONITORING_EMULATOR_ADMIN_ADDR"); v != "" {
		return v
	}
	return "localhost:9091"
}

// repoRoot returns the absolute path to the repository root.
func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// createSpannerInstance creates a Spanner instance in the emulator via the admin HTTP API.
func createSpannerInstance(t *testing.T, projectID, instanceID string, processingUnits int) {
	t.Helper()
	body, _ := json.Marshal(map[string]interface{}{"processing_units": processingUnits})
	url := fmt.Sprintf("http://%s/instances/%s/%s", spannerAdminAddr(), projectID, instanceID)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("createSpannerInstance: failed to build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("createSpannerInstance %s: %v", url, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("createSpannerInstance %s: unexpected status %d", url, resp.StatusCode)
	}
}

// startEnvtest starts an envtest environment and returns a controller-runtime client.
// The environment is stopped via t.Cleanup.
func startEnvtest(t *testing.T) ctrlclient.Client {
	t.Helper()
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(repoRoot(), "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("failed to start envtest: %v", err)
	}
	t.Cleanup(func() { testEnv.Stop() }) //nolint:errcheck

	if err := spannerv1alpha1.AddToScheme(k8sscheme.Scheme); err != nil {
		t.Fatalf("failed to add v1alpha1 scheme: %v", err)
	}
	if err := spannerv1beta1.AddToScheme(k8sscheme.Scheme); err != nil {
		t.Fatalf("failed to add v1beta1 scheme: %v", err)
	}

	k8sClient, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: k8sscheme.Scheme})
	if err != nil {
		t.Fatalf("failed to create k8s client: %v", err)
	}
	return k8sClient
}

// adminPUT sends a PUT request to the monitoring emulator admin API.
func adminPUT(t *testing.T, path string, body []byte) {
	t.Helper()
	url := fmt.Sprintf("http://%s%s", monitoringAdminAddr(), path)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("adminPUT: failed to build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("adminPUT %s: %v", url, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("adminPUT %s: unexpected status %d", url, resp.StatusCode)
	}
}

// adminDELETE sends a DELETE request to the monitoring emulator admin API.
func adminDELETE(t *testing.T, path string) {
	t.Helper()
	url := fmt.Sprintf("http://%s%s", monitoringAdminAddr(), path)
	req, err := http.NewRequest(http.MethodDelete, url, nil)
	if err != nil {
		t.Fatalf("adminDELETE: failed to build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("adminDELETE %s: %v", url, err)
	}
	resp.Body.Close()
}
