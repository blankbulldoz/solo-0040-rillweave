package wire

import (
	"net/http"

	"rillweave/change"
	"rillweave/engine"
	"rillweave/graph"
	"rillweave/image"
)

func (s service) create(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Name   string       `json:"name"`
		Schema graph.Schema `json:"schema"`
	}
	if err := decode(w, r, &q, maxRequest); err != nil {
		result(w, nil, err, 0)
		return
	}
	published, err := s.db.Create(q.Name, q.Schema)
	result(w, published, err, http.StatusCreated)
}

func (s service) commit(w http.ResponseWriter, r *http.Request) {
	var q change.Batch
	if err := decode(w, r, &q, maxRequest); err != nil {
		result(w, nil, err, 0)
		return
	}
	published, err := s.db.Commit(r.PathValue("name"), q)
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
	var q struct {
		image.Envelope
		Operation string  `json:"operation,omitempty"`
		Expect    *uint64 `json:"expect,omitempty"`
	}
	if err := decode(w, r, &q, image.MaxImage); err != nil {
		result(w, nil, err, 0)
		return
	}
	receipt, err := s.db.Restore(engine.RestoreRequest{
		Name: r.PathValue("name"), Envelope: q.Envelope,
		Operation: q.Operation, Expected: q.Expect,
	})
	status := http.StatusOK
	if receipt.Created {
		status = http.StatusCreated
	}
	result(w, receipt, err, status)
}
