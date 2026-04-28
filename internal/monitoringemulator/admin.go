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
//	  Body (per-metric, recommended for dual CPU scaling testing):
//	    {"high_priority": 0.65, "total": 0.45}
//	  Body (legacy, sets both metrics to the same value):
//	    {"cpu_utilization": 0.45}
//	GET    /metrics/{project_id}/{instance_id}
//	DELETE /metrics/{project_id}/{instance_id}
//
// Dynamic mode endpoints (workload-based CPU calculation):
//
//	PUT    /workload/{project_id}/{instance_id}
//	  Body (per-metric, recommended for dual CPU scaling testing):
//	    {"high_priority": {"cpu_utilization": 0.80, "reference_processing_units": 1000},
//	     "total":         {"cpu_utilization": 0.50, "reference_processing_units": 1000}}
//	  Body (legacy, sets both metrics to the same workload):
//	    {"cpu_utilization": 0.80, "reference_processing_units": 1000}
//	GET    /workload/{project_id}/{instance_id}
//	DELETE /workload/{project_id}/{instance_id}
//
// Scenario mode endpoints (time-based step sequence, loops indefinitely):
//
//	PUT    /scenario/{project_id}/{instance_id}
//	  Body (per-metric step):
//	    {"steps": [{"duration": "30s",
//	                "high_priority": {"cpu_utilization": 0.80},
//	                "total":         {"cpu_utilization": 0.50}}, ...]}
//	  Body (legacy step, applies same value to both metrics):
//	    {"steps": [{"duration": "30s", "cpu_utilization": 0.80}, ...]}
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

// staticSetRequest accepts either per-metric fields or the legacy cpu_utilization field.
// Per-metric fields (high_priority, total) take precedence over the legacy field.
// At least one field must be set.
type staticSetRequest struct {
	// Legacy: sets both highPriority and total to the same value.
	CPUUtilization *float64 `json:"cpu_utilization,omitempty"`
	// Per-metric values (recommended for dual CPU scaling testing).
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

		if req.CPUUtilization == nil && req.HighPriority == nil && req.Total == nil {
			http.Error(w, "must set cpu_utilization, high_priority, or total", http.StatusBadRequest)
			return
		}

		entry := CPUEntry{}
		if req.HighPriority != nil || req.Total != nil {
			// Per-metric mode.
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
		} else {
			// Legacy mode: apply the same value to both metrics.
			if err := validateCPUField("cpu_utilization", req.CPUUtilization); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			entry.HighPriority = req.CPUUtilization
			entry.Total = req.CPUUtilization
		}

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

// workloadSetRequest accepts either per-metric fields or the legacy flat fields.
// Per-metric fields (high_priority, total) take precedence over the legacy fields.
type workloadSetRequest struct {
	// Legacy: sets both metrics to the same workload.
	CPUUtilization           *float64 `json:"cpu_utilization,omitempty"`
	ReferenceProcessingUnits int      `json:"reference_processing_units,omitempty"`
	// Per-metric workloads (recommended for dual CPU scaling testing).
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

		if req.CPUUtilization == nil && req.HighPriority == nil && req.Total == nil {
			http.Error(w, "must set cpu_utilization or per-metric high_priority/total", http.StatusBadRequest)
			return
		}

		entry := WorkloadEntry{}
		if req.HighPriority != nil || req.Total != nil {
			// Per-metric mode.
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
		} else {
			// Legacy mode: apply the same workload to both metrics.
			if err := validateWorkloadMetric("", *req.CPUUtilization, req.ReferenceProcessingUnits); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			p := newWorkloadParams(*req.CPUUtilization, req.ReferenceProcessingUnits)
			entry.HighPriority = &p
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
