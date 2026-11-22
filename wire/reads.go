package wire

import (
	"net/http"

	"rillweave/change"
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

func (s service) catalog(w http.ResponseWriter, r *http.Request) {
	value, err := s.db.Catalog(r.PathValue("name"), selected(r))
	result(w, value, err, http.StatusOK)
}

func (s service) vertex(w http.ResponseWriter, r *http.Request) {
	value, err := s.db.Vertex(r.PathValue("name"), selected(r), r.PathValue("id"))
	result(w, value, err, http.StatusOK)
}

func (s service) compare(w http.ResponseWriter, r *http.Request) {
	var q change.Query
	if err := decode(w, r, &q, maxRequest); err != nil {
		result(w, nil, err, 0)
		return
	}
	value, err := s.db.Comparison(r.PathValue("name"), q)
	result(w, value, err, http.StatusOK)
}

func (s service) export(w http.ResponseWriter, r *http.Request) {
	value, err := s.db.Export(r.PathValue("name"), selected(r))
	result(w, value, err, http.StatusOK)
}
