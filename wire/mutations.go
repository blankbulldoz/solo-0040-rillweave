package wire

import (
	"net/http"

	"rillweave/change"
	"rillweave/graph"
	"rillweave/image"
)

func (s service) create(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Name   string       `json:"name"`
		Schema graph.Schema `json:"schema"`
	}
	if err := decode(w, r, &q, MaxRequest); err != nil {
		result(w, nil, err, 0)
		return
	}
	published, err := s.db.Create(q.Name, q.Schema)
	result(w, published, err, http.StatusCreated)
}

func (s service) commit(w http.ResponseWriter, r *http.Request) {
	var q change.Batch
	if err := decode(w, r, &q, MaxRequest); err != nil {
		result(w, nil, err, 0)
		return
	}
	published, err := s.db.Commit(r.PathValue("name"), q)
	result(w, published, err, http.StatusOK)
}

// commitV2 accepts the typed operation envelope form of a commit. Decoding
// into the shared domain batch keeps one commit path for both entries.
func (s service) commitV2(w http.ResponseWriter, r *http.Request) {
	var q change.EnvelopeBatch
	if err := decode(w, r, &q, MaxRequest); err != nil {
		result(w, nil, err, 0)
		return
	}
	batch, err := q.Batch()
	if err != nil {
		result(w, nil, err, 0)
		return
	}
	published, err := s.db.Commit(r.PathValue("name"), batch)
	result(w, published, err, http.StatusOK)
}

func (s service) hold(w http.ResponseWriter, r *http.Request) {
	held, err := s.db.Hold(r.PathValue("name"))
	result(w, held, err, http.StatusCreated)
}

func (s service) release(w http.ResponseWriter, r *http.Request) {
	err := s.db.Release(r.PathValue("name"), r.PathValue("handle"))
	result(w, nil, err, http.StatusNoContent)
}

func (s service) restore(w http.ResponseWriter, r *http.Request) {
	var e image.Envelope
	if err := decode(w, r, &e, image.MaxImage); err != nil {
		result(w, nil, err, 0)
		return
	}
	receipt, err := s.db.Restore(r.PathValue("name"), e)
	result(w, receipt, err, http.StatusCreated)
}
