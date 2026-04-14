package spanneremulator

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// NewAdminHandler returns an HTTP handler for the Spanner emulator admin API.
//
//	PUT    /instances/{project_id}/{instance_id}   {"processing_units": 1000}
//	DELETE /instances/{project_id}/{instance_id}
func NewAdminHandler(srv *Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /instances/{project_id}/{instance_id}", handleUpsert(srv))
	mux.HandleFunc("DELETE /instances/{project_id}/{instance_id}", handleDelete(srv))
	return mux
}

type upsertRequest struct {
	ProcessingUnits int32 `json:"processing_units"`
}

type instanceResponse struct {
	Name            string `json:"name"`
	ProcessingUnits int32  `json:"processing_units"`
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
		if req.ProcessingUnits <= 0 {
			http.Error(w, "processing_units must be greater than 0", http.StatusBadRequest)
			return
		}

		name := fmt.Sprintf("projects/%s/instances/%s", projectID, instanceID)
		srv.AdminUpsertInstance(name, req.ProcessingUnits)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(instanceResponse{ //nolint:errcheck,gosec
			Name:            name,
			ProcessingUnits: req.ProcessingUnits,
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
