package wire

import (
	"net/http"

	"rillweave/change"
)

type rehearsalRequest struct {
	change.Batch
	// Baseline is an optional pin handle. Omitting it rehearses against the
	// current head; supplying it lets an older revision be rehearsed after the
	// head has advanced.
	Baseline string `json:"baseline,omitempty"`
}

func (s service) rehearse(w http.ResponseWriter, r *http.Request) {
	var q rehearsalRequest
	if err := decode(w, r, &q, maxRequest); err != nil {
		result(w, nil, err, 0)
		return
	}
	receipt, err := s.db.Rehearse(r.PathValue("name"), q.Batch, q.Baseline)
	result(w, receipt, err, http.StatusCreated)
}

func (s service) discard(w http.ResponseWriter, r *http.Request) {
	err := s.db.Discard(r.PathValue("name"), r.PathValue("handle"))
	result(w, nil, err, http.StatusNoContent)
}
