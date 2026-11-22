package wire

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"rillweave/engine"
	"rillweave/graph"
	"rillweave/image"
)

const maxRequest = 8 << 20

type service struct {
	db *engine.Database
}

func New(db *engine.Database) http.Handler {
	mux := http.NewServeMux()
	s := service{db}
	mux.HandleFunc("GET /ready", s.ready)
	mux.HandleFunc("POST /rill/v1/graphs", s.create)
	mux.HandleFunc("GET /rill/v1/graphs/{name}/catalog", s.catalog)
	mux.HandleFunc("POST /rill/v1/graphs/{name}/commit", s.commit)
	mux.HandleFunc("GET /rill/v1/graphs/{name}/vertices/{id}", s.vertex)
	mux.HandleFunc("POST /rill/v1/graphs/{name}/lookup", reading(db.Lookup))
	mux.HandleFunc("POST /rill/v1/graphs/{name}/adjacency", reading(db.Adjacent))
	mux.HandleFunc("POST /rill/v1/graphs/{name}/reach", reading(db.Reach))
	mux.HandleFunc("POST /rill/v1/graphs/{name}/path", reading(db.Path))
	mux.HandleFunc("POST /rill/v1/graphs/{name}/components", reading(db.Components))
	mux.HandleFunc("POST /rill/v1/graphs/{name}/census", reading(db.Census))
	mux.HandleFunc("POST /rill/v1/graphs/{name}/census/evidence", reading(db.CensusEvidence))
	mux.HandleFunc("POST /rill/v1/graphs/{name}/pins", s.hold)
	mux.HandleFunc("DELETE /rill/v1/graphs/{name}/pins/{handle}", s.release)
	mux.HandleFunc("POST /rill/v1/graphs/{name}/compare", s.compare)
	mux.HandleFunc("GET /rill/v1/graphs/{name}/image", s.export)
	mux.HandleFunc("PUT /rill/v1/graphs/{name}/image", s.restore)
	return mux
}

func decode(w http.ResponseWriter, r *http.Request, dst any, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := image.ReadJSON(r.Body, dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return graph.Fail(graph.Budget, "request exceeds %d bytes", limit)
		}
		return graph.Fail(graph.Invalid, "invalid JSON request: %v", err)
	}
	return nil
}

func result(w http.ResponseWriter, value any, err error, status int) {
	if err != nil {
		var problem *graph.Problem
		if !errors.As(err, &problem) {
			log.Printf("database operation: %v", err)
			problem = &graph.Problem{Code: graph.Unavailable, Details: "database operation could not complete"}
		}
		status = codeStatus(problem.Code)
		value = problem
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status != http.StatusNoContent {
		if err := json.NewEncoder(w).Encode(value); err != nil {
			log.Printf("write response: %v", err)
		}
	}
}

func codeStatus(code graph.Code) int {
	switch code {
	case graph.Invalid:
		return http.StatusBadRequest
	case graph.Missing:
		return http.StatusNotFound
	case graph.Conflict:
		return http.StatusConflict
	case graph.Budget:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusServiceUnavailable
	}
}

func selected(r *http.Request) string {
	return r.URL.Query().Get("view")
}

func (s service) ready(w http.ResponseWriter, r *http.Request) {
	result(w, struct {
		Status string `json:"status"`
	}{"ready"}, s.db.Ready(), http.StatusOK)
}
