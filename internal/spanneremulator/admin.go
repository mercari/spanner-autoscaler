package spanneremulator

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// NewAdminHandler returns an HTTP handler for the Spanner emulator admin API.
//
//	PUT    /instances/{project_id}/{instance_id}   {"processing_units": 1000, "config": "projects/p/instanceConfigs/regional-asia-northeast1"}
//	DELETE /instances/{project_id}/{instance_id}
//	PUT    /quota/{project_id}/{config_id}         {"processing_units": 100000}
//	DELETE /quota/{project_id}/{config_id}
//
// processingUnits is also accepted for Kubernetes-style JSON payloads.
func NewAdminHandler(srv *Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /instances/{project_id}/{instance_id}", handleUpsert(srv))
	mux.HandleFunc("DELETE /instances/{project_id}/{instance_id}", handleDelete(srv))
	mux.HandleFunc("PUT /quota/{project_id}/{config_id}", handleQuotaSet(srv))
	mux.HandleFunc("DELETE /quota/{project_id}/{config_id}", handleQuotaDelete(srv))
	return mux
}

type upsertRequest struct {
	ProcessingUnits      int32  `json:"processing_units"`
	ProcessingUnitsCamel int32  `json:"processingUnits"`
	Config               string `json:"config"`
}

type instanceResponse struct {
	Name            string `json:"name"`
	ProcessingUnits int32  `json:"processing_units"`
	Config          string `json:"config"`
}

func handleUpsert(srv *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectID := r.PathValue("project_id")
		instanceID := r.PathValue("instance_id")

		var req upsertRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		processingUnits := req.processingUnits()
		if processingUnits <= 0 {
			http.Error(w, "processing_units must be greater than 0", http.StatusBadRequest)
			return
		}

		name := fmt.Sprintf("projects/%s/instances/%s", projectID, instanceID)
		config := req.Config
		if config == "" {
			config = fmt.Sprintf("projects/%s/instanceConfigs/regional-asia-northeast1", projectID)
		}
		srv.AdminUpsertInstance(name, processingUnits, config)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(instanceResponse{ //nolint:errcheck,gosec
			Name:            name,
			ProcessingUnits: processingUnits,
			Config:          config,
		})
	}
}

func handleDelete(srv *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectID := r.PathValue("project_id")
		instanceID := r.PathValue("instance_id")
		name := fmt.Sprintf("projects/%s/instances/%s", projectID, instanceID)
		srv.AdminDeleteInstance(name)
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleQuotaSet(srv *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectID := r.PathValue("project_id")
		configID := r.PathValue("config_id")

		var req upsertRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
			return
		}
		processingUnits := req.processingUnits()
		if processingUnits <= 0 {
			http.Error(w, "processing_units must be greater than 0", http.StatusBadRequest)
			return
		}

		srv.AdminSetQuota(projectID, configID, processingUnits)
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleQuotaDelete(srv *Server) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		srv.AdminDeleteQuota(r.PathValue("project_id"), r.PathValue("config_id"))
		w.WriteHeader(http.StatusNoContent)
	}
}

func (r upsertRequest) processingUnits() int32 {
	if r.ProcessingUnits != 0 {
		return r.ProcessingUnits
	}
	return r.ProcessingUnitsCamel
}
