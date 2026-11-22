package wire

import (
	"net/http"

	"rillweave/compress"
)

func (s service) compress(w http.ResponseWriter, r *http.Request) {
	var req compress.Request
	if err := decode(w, r, &req, maxRequest); err != nil {
		result(w, nil, err, 0)
		return
	}
	value, err := s.db.Compress(r.PathValue("name"), selected(r), req)
	result(w, value, err, http.StatusOK)
}

func (s service) installCompact(w http.ResponseWriter, r *http.Request) {
	var envelope compress.Envelope
	if err := decode(w, r, &envelope, compress.MaxDocument); err != nil {
		result(w, nil, err, 0)
		return
	}
	receipt, err := s.db.InstallCompressed(envelope)
	result(w, receipt, err, http.StatusCreated)
}

func (s service) compactOverview(w http.ResponseWriter, r *http.Request) {
	overview, err := s.db.CompactOverview(r.PathValue("name"))
	result(w, overview, err, http.StatusOK)
}

func (s service) compactImage(w http.ResponseWriter, r *http.Request) {
	envelope, err := s.db.CompactDocument(r.PathValue("name"))
	result(w, envelope, err, http.StatusOK)
}

func (s service) compactRoute(w http.ResponseWriter, r *http.Request) {
	var req compress.RouteRequest
	if err := decode(w, r, &req, maxRequest); err != nil {
		result(w, nil, err, 0)
		return
	}
	answer, err := s.db.CompactRoute(r.PathValue("name"), req)
	result(w, answer, err, http.StatusOK)
}

func (s service) compactReplay(w http.ResponseWriter, r *http.Request) {
	var q struct {
		Arcs []string `json:"arcs"`
	}
	if err := decode(w, r, &q, maxRequest); err != nil {
		result(w, nil, err, 0)
		return
	}
	verification, err := s.db.CompactReplay(r.PathValue("name"), q.Arcs)
	result(w, verification, err, http.StatusOK)
}
