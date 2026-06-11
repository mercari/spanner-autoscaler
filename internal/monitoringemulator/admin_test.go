package monitoringemulator

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
)

func newTestAdminHandler() (*QuotaModeStore, http.Handler) {
	staticStore := NewStaticStore()
	workloadStore := NewWorkloadStore()
	scenarioStore := NewScenarioStore()
	quotaStore := NewQuotaStore()
	quotaModeStore := NewQuotaModeStore()
	return quotaModeStore, NewAdminHandler(staticStore, workloadStore, scenarioStore, quotaStore, quotaModeStore)
}

func TestAdmin_QuotaMode_SetAndDelete(t *testing.T) {
	store, handler := newTestAdminHandler()

	req := httptest.NewRequest(http.MethodPut, "/quota-mode",
		strings.NewReader(`{"grpcCode":"PermissionDenied","message":"denied"}`))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("PUT /quota-mode status = %d, want %d; body=%q", rec.Code, http.StatusNoContent, rec.Body.String())
	}
	mode, ok := store.Get()
	if !ok {
		t.Fatal("store should have a mode set")
	}
	if mode.Code != codes.PermissionDenied || mode.Message != "denied" {
		t.Errorf("stored mode = %+v, want PermissionDenied/denied", mode)
	}

	req = httptest.NewRequest(http.MethodDelete, "/quota-mode", http.NoBody)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE /quota-mode status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if _, ok := store.Get(); ok {
		t.Error("store should be cleared after DELETE")
	}
}

func TestAdmin_QuotaMode_BadRequests(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "invalid json", body: `{not json`},
		{name: "missing grpcCode", body: `{"message":"x"}`},
		{name: "empty grpcCode", body: `{"grpcCode":"","message":"x"}`},
		{name: "unknown grpcCode", body: `{"grpcCode":"NotARealCode"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, handler := newTestAdminHandler()
			req := httptest.NewRequest(http.MethodPut, "/quota-mode", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
			}
			if _, ok := store.Get(); ok {
				t.Errorf("store should remain empty after a bad request")
			}
		})
	}
}
