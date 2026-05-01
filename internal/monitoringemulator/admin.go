package monitoringemulator

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// NewAdminHandler returns an HTTP handler for the monitoring emulator admin API.
//
// Static mode endpoints (fixed CPU utilization):
//
//	PUT    /metrics/{project_id}/{instance_id}
//	  Body: {"high_priority": 0.65, "total": 0.45}
//	  At least one of high_priority or total must be set.
//	GET    /metrics/{project_id}/{instance_id}
//	DELETE /metrics/{project_id}/{instance_id}
//
// Dynamic mode endpoints (workload-based CPU calculation):
//
//	PUT    /workload/{project_id}/{instance_id}
//	  Body: {"high_priority": {"cpu_utilization": 0.80, "reference_processing_units": 1000},
//	         "total":         {"cpu_utilization": 0.50, "reference_processing_units": 1000}}
//	  At least one of high_priority or total must be set.
//	GET    /workload/{project_id}/{instance_id}
//	DELETE /workload/{project_id}/{instance_id}
//
// Scenario mode endpoints (time-based step sequence, loops indefinitely):
//
//	PUT    /scenario/{project_id}/{instance_id}
//	  Body: {"steps": [{"duration": "30s",
//	                    "high_priority": {"cpu_utilization": 0.80},
//	                    "total":         {"cpu_utilization": 0.50}}, ...]}
//	  At least one of high_priority or total must be set per step.
//	DELETE /scenario/{project_id}/{instance_id}
func NewAdminHandler(staticStore *StaticStore, workloadStore *WorkloadStore, scenarioStore *ScenarioStore) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("PUT /metrics/{project_id}/{instance_id}", handleStaticSet(staticStore))
	mux.HandleFunc("GET /metrics/{project_id}/{instance_id}", handleStaticGet(staticStore))
	mux.HandleFunc("DELETE /metrics/{project_id}/{instance_id}", handleStaticDelete(staticStore))

	mux.HandleFunc("PUT /workload/{project_id}/{instance_id}", handleWorkloadSet(workloadStore))
	mux.HandleFunc("GET /workload/{project_id}/{instance_id}", handleWorkloadGet(workloadStore))
	mux.HandleFunc("DELETE /workload/{project_id}/{instance_id}", handleWorkloadDelete(workloadStore))

	mux.HandleFunc("PUT /scenario/{project_id}/{instance_id}", handleScenarioSet(scenarioStore))
	mux.HandleFunc("DELETE /scenario/{project_id}/{instance_id}", handleScenarioDelete(scenarioStore))

	return mux
}

// ---- static mode ----

// staticSetRequest sets independent fixed CPU values per metric type.
// At least one of HighPriority or Total must be set.
type staticSetRequest struct {
	HighPriority *float64 `json:"high_priority,omitempty"`
	Total        *float64 `json:"total,omitempty"`
}

type staticResponse struct {
	ProjectID    string   `json:"project_id"`
	InstanceID   string   `json:"instance_id"`
	HighPriority *float64 `json:"high_priority,omitempty"`
	Total        *float64 `json:"total,omitempty"`
}

func handleStaticSet(store *StaticStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectID := r.PathValue("project_id")
		instanceID := r.PathValue("instance_id")

		var req staticSetRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}

		if req.HighPriority == nil && req.Total == nil {
			http.Error(w, "must set high_priority and/or total", http.StatusBadRequest)
			return
		}

		entry := CPUEntry{}
		if err := validateCPUField("high_priority", req.HighPriority); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := validateCPUField("total", req.Total); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		entry.HighPriority = req.HighPriority
		entry.Total = req.Total

		store.Set(projectID, instanceID, entry)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(staticResponse{ //nolint:errcheck,gosec
			ProjectID:    projectID,
			InstanceID:   instanceID,
			HighPriority: entry.HighPriority,
			Total:        entry.Total,
		})
	}
}

func handleStaticGet(store *StaticStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectID := r.PathValue("project_id")
		instanceID := r.PathValue("instance_id")

		entry, ok := store.GetEntry(projectID, instanceID)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(staticResponse{ //nolint:errcheck,gosec
			ProjectID:    projectID,
			InstanceID:   instanceID,
			HighPriority: entry.HighPriority,
			Total:        entry.Total,
		})
	}
}

func handleStaticDelete(store *StaticStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store.Delete(r.PathValue("project_id"), r.PathValue("instance_id"))
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---- dynamic (workload) mode ----

// workloadMetricRequest holds workload parameters for one CPU metric type.
type workloadMetricRequest struct {
	CPUUtilization           float64 `json:"cpu_utilization"`
	ReferenceProcessingUnits int     `json:"reference_processing_units"`
}

// workloadSetRequest sets independent workload parameters per metric type.
// At least one of HighPriority or Total must be set.
type workloadSetRequest struct {
	HighPriority *workloadMetricRequest `json:"high_priority,omitempty"`
	Total        *workloadMetricRequest `json:"total,omitempty"`
}

type workloadMetricResponse struct {
	Workload                 float64 `json:"workload"`
	ReferenceCPUUtilization  float64 `json:"reference_cpu_utilization"`
	ReferenceProcessingUnits int     `json:"reference_processing_units"`
}

type workloadResponse struct {
	ProjectID    string                  `json:"project_id"`
	InstanceID   string                  `json:"instance_id"`
	HighPriority *workloadMetricResponse `json:"high_priority,omitempty"`
	Total        *workloadMetricResponse `json:"total,omitempty"`
}

func workloadParamsToResponse(p *WorkloadParams) *workloadMetricResponse {
	if p == nil {
		return nil
	}
	return &workloadMetricResponse{
		Workload:                 p.Workload,
		ReferenceCPUUtilization:  p.ReferenceCPU,
		ReferenceProcessingUnits: p.ReferencePU,
	}
}

func handleWorkloadSet(store *WorkloadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectID := r.PathValue("project_id")
		instanceID := r.PathValue("instance_id")

		var req workloadSetRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}

		if req.HighPriority == nil && req.Total == nil {
			http.Error(w, "must set high_priority and/or total", http.StatusBadRequest)
			return
		}

		entry := WorkloadEntry{}
		if req.HighPriority != nil {
			if err := validateWorkloadMetric("high_priority", req.HighPriority.CPUUtilization, req.HighPriority.ReferenceProcessingUnits); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			p := newWorkloadParams(req.HighPriority.CPUUtilization, req.HighPriority.ReferenceProcessingUnits)
			entry.HighPriority = &p
		}
		if req.Total != nil {
			if err := validateWorkloadMetric("total", req.Total.CPUUtilization, req.Total.ReferenceProcessingUnits); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			p := newWorkloadParams(req.Total.CPUUtilization, req.Total.ReferenceProcessingUnits)
			entry.Total = &p
		}

		store.Set(projectID, instanceID, entry)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(workloadResponse{ //nolint:errcheck,gosec
			ProjectID:    projectID,
			InstanceID:   instanceID,
			HighPriority: workloadParamsToResponse(entry.HighPriority),
			Total:        workloadParamsToResponse(entry.Total),
		})
	}
}

func handleWorkloadGet(store *WorkloadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectID := r.PathValue("project_id")
		instanceID := r.PathValue("instance_id")

		entry, ok := store.GetEntry(projectID, instanceID)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(workloadResponse{ //nolint:errcheck,gosec
			ProjectID:    projectID,
			InstanceID:   instanceID,
			HighPriority: workloadParamsToResponse(entry.HighPriority),
			Total:        workloadParamsToResponse(entry.Total),
		})
	}
}

func handleWorkloadDelete(store *WorkloadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store.Delete(r.PathValue("project_id"), r.PathValue("instance_id"))
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---- scenario mode ----

type scenarioSetRequest struct {
	Steps []ScenarioStep `json:"steps"`
}

func handleScenarioSet(store *ScenarioStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectID := r.PathValue("project_id")
		instanceID := r.PathValue("instance_id")

		var req scenarioSetRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := store.Set(projectID, instanceID, req.Steps); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func handleScenarioDelete(store *ScenarioStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		store.Delete(r.PathValue("project_id"), r.PathValue("instance_id"))
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---- validation helpers ----

func validateCPUField(name string, v *float64) error {
	if v == nil {
		return nil
	}
	if *v < 0 || *v > 1 {
		if name != "" {
			return fmt.Errorf("%s must be between 0.0 and 1.0", name)
		}
		return fmt.Errorf("cpu_utilization must be between 0.0 and 1.0")
	}
	return nil
}

func validateWorkloadMetric(name string, cpu float64, pu int) error {
	if cpu < 0 || cpu > 1 {
		if name != "" {
			return fmt.Errorf("%s cpu_utilization must be between 0.0 and 1.0", name)
		}
		return fmt.Errorf("cpu_utilization must be between 0.0 and 1.0")
	}
	if pu <= 0 {
		if name != "" {
			return fmt.Errorf("%s reference_processing_units must be greater than 0", name)
		}
		return fmt.Errorf("reference_processing_units must be greater than 0")
	}
	return nil
}
