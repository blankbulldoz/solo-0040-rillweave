package wire

import (
	"net/http"

	"rillweave/handoff"
)

func (s service) handoff(w http.ResponseWriter, r *http.Request) {
	var q handoff.Request
	if err := decode(w, r, &q, maxRequest); err != nil {
		result(w, nil, err, 0)
		return
	}
	receipt, err := s.db.Handoff(r.PathValue("name"), selected(r), q)
	result(w, receipt, err, http.StatusCreated)
}

func (s service) handoffManifest(w http.ResponseWriter, r *http.Request) {
	value, err := s.db.HandoffManifest(r.PathValue("name"), selected(r))
	result(w, value, err, http.StatusOK)
}
