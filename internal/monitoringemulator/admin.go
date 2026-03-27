package monitoringemulator

import (
	"encoding/json"
	"net/http"
)

// NewAdminHandler returns an HTTP handler for the monitoring emulator admin API.
//
// Static mode endpoints (fixed CPU utilization):
//
//	PUT    /metrics/{project_id}/{instance_id}   {"cpu_utilization": 0.45}
//	GET    /metrics/{project_id}/{instance_id}
//	DELETE /metrics/{project_id}/{instance_id}
//
// Dynamic mode endpoints (workload-based CPU calculation):
//
//	PUT    /workload/{project_id}/{instance_id}  {"cpu_utilization": 0.80, "reference_processing_units": 1000}
//	GET    /workload/{project_id}/{instance_id}
//	DELETE /workload/{project_id}/{instance_id}
//
// Scenario mode endpoints (time-based step sequence, loops indefinitely):
//
//	PUT    /scenario/{project_id}/{instance_id}  {"steps": [{"duration": "30s", "cpu_utilization": 0.80}, ...]}
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

type staticSetRequest struct {
	CPUUtilization float64 `json:"cpu_utilization"`
}

type staticResponse struct {
	ProjectID      string  `json:"project_id"`
	InstanceID     string  `json:"instance_id"`
	CPUUtilization float64 `json:"cpu_utilization"`
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
		if req.CPUUtilization < 0 || req.CPUUtilization > 1 {
			http.Error(w, "cpu_utilization must be between 0.0 and 1.0", http.StatusBadRequest)
			return
		}

		store.Set(projectID, instanceID, req.CPUUtilization)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(staticResponse{ //nolint:errcheck
			ProjectID:      projectID,
			InstanceID:     instanceID,
			CPUUtilization: req.CPUUtilization,
		})
	}
}

func handleStaticGet(store *StaticStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectID := r.PathValue("project_id")
		instanceID := r.PathValue("instance_id")

		cpu, ok := store.Get(projectID, instanceID)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(staticResponse{ //nolint:errcheck
			ProjectID:      projectID,
			InstanceID:     instanceID,
			CPUUtilization: cpu,
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

type workloadSetRequest struct {
	CPUUtilization           float64 `json:"cpu_utilization"`
	ReferenceProcessingUnits int     `json:"reference_processing_units"`
}

type workloadResponse struct {
	ProjectID                string  `json:"project_id"`
	InstanceID               string  `json:"instance_id"`
	Workload                 float64 `json:"workload"`
	ReferenceCPUUtilization  float64 `json:"reference_cpu_utilization"`
	ReferenceProcessingUnits int     `json:"reference_processing_units"`
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
		if req.CPUUtilization < 0 || req.CPUUtilization > 1 {
			http.Error(w, "cpu_utilization must be between 0.0 and 1.0", http.StatusBadRequest)
			return
		}
		if req.ReferenceProcessingUnits <= 0 {
			http.Error(w, "reference_processing_units must be greater than 0", http.StatusBadRequest)
			return
		}

		store.Set(projectID, instanceID, req.CPUUtilization, req.ReferenceProcessingUnits)
		entry, _ := store.Get(projectID, instanceID)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(workloadResponse{ //nolint:errcheck
			ProjectID:                projectID,
			InstanceID:               instanceID,
			Workload:                 entry.Workload,
			ReferenceCPUUtilization:  entry.ReferenceCPU,
			ReferenceProcessingUnits: entry.ReferencePU,
		})
	}
}

func handleWorkloadGet(store *WorkloadStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectID := r.PathValue("project_id")
		instanceID := r.PathValue("instance_id")

		entry, ok := store.Get(projectID, instanceID)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(workloadResponse{ //nolint:errcheck
			ProjectID:                projectID,
			InstanceID:               instanceID,
			Workload:                 entry.Workload,
			ReferenceCPUUtilization:  entry.ReferenceCPU,
			ReferenceProcessingUnits: entry.ReferencePU,
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
