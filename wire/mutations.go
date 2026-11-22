package wire

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"time"

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

// leaseRequest carries an optional finite lease. An absent body or a zero
// duration selects the original indefinite pin, which only retires on an
// explicit release.
type leaseRequest struct {
	TTLMillis int64 `json:"ttl_millis,omitempty"`
}

func decodeLease(w http.ResponseWriter, r *http.Request) (time.Duration, error) {
	var q leaseRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxRequest)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return 0, graph.Fail(graph.Budget, "request exceeds %d bytes", maxRequest)
		}
		return 0, graph.Fail(graph.Invalid, "invalid request body: %v", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return 0, nil
	}
	if err := image.ReadJSON(bytes.NewReader(raw), &q); err != nil {
		return 0, graph.Fail(graph.Invalid, "invalid JSON request: %v", err)
	}
	return time.Duration(q.TTLMillis) * time.Millisecond, nil
}

func (s service) hold(w http.ResponseWriter, r *http.Request) {
	ttl, err := decodeLease(w, r)
	if err != nil {
		result(w, nil, err, 0)
		return
	}
	held, err := s.db.Hold(r.PathValue("name"), engine.PinLease{TTL: ttl})
	result(w, held, err, http.StatusCreated)
}

func (s service) renew(w http.ResponseWriter, r *http.Request) {
	ttl, err := decodeLease(w, r)
	if err != nil {
		result(w, nil, err, 0)
		return
	}
	held, err := s.db.Renew(r.PathValue("name"), r.PathValue("handle"), ttl)
	result(w, held, err, http.StatusOK)
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
