package wire

import (
	"net/http"

	"rillweave/walk"
)

func reading[Q, R any](read func(string, string, Q) (R, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var q Q
		if err := decode(w, r, &q, maxRequest); err != nil {
			result(w, nil, err, 0)
			return
		}
		value, err := read(r.PathValue("name"), selected(r), q)
		result(w, value, err, http.StatusOK)
	}
}

// pathRequest carries the optional session token that resumes a paused search.
type pathRequest struct {
	walk.PathRequest
	Session string `json:"session,omitempty"`
}

func (s service) path(w http.ResponseWriter, r *http.Request) {
	var q pathRequest
	if err := decode(w, r, &q, maxRequest); err != nil {
		result(w, nil, err, 0)
		return
	}
	value, err := s.db.Path(r.PathValue("name"), selected(r), q.PathRequest, q.Session)
	result(w, value, err, http.StatusOK)
}

func (s service) releasePath(w http.ResponseWriter, r *http.Request) {
	err := s.db.ReleasePath(r.PathValue("name"), r.PathValue("session"))
	result(w, nil, err, http.StatusNoContent)
}

func (s service) catalog(w http.ResponseWriter, r *http.Request) {
	value, err := s.db.Catalog(r.PathValue("name"), selected(r))
	result(w, value, err, http.StatusOK)
}

func (s service) vertex(w http.ResponseWriter, r *http.Request) {
	value, err := s.db.Vertex(r.PathValue("name"), selected(r), r.PathValue("id"))
	result(w, value, err, http.StatusOK)
}

func (s service) compare(w http.ResponseWriter, r *http.Request) {
	var q struct {
		From string `json:"from,omitempty"`
		To   string `json:"to,omitempty"`
	}
	if err := decode(w, r, &q, maxRequest); err != nil {
		result(w, nil, err, 0)
		return
	}
	value, err := s.db.Comparison(r.PathValue("name"), q.From, q.To)
	result(w, value, err, http.StatusOK)
}

func (s service) export(w http.ResponseWriter, r *http.Request) {
	value, err := s.db.Export(r.PathValue("name"), selected(r))
	result(w, value, err, http.StatusOK)
}
