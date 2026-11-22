package wire

import (
	"net/http"

	"rillweave/walk"
)

func (s service) openReach(w http.ResponseWriter, r *http.Request) {
	var q walk.ReachPageRequest
	if err := decode(w, r, &q, maxRequest); err != nil {
		result(w, nil, err, 0)
		return
	}
	value, err := s.db.OpenReachSession(r.PathValue("name"), selected(r), q)
	result(w, value, err, http.StatusCreated)
}

func (s service) advanceReach(w http.ResponseWriter, r *http.Request) {
	var q walk.ReachContinue
	if err := decode(w, r, &q, maxRequest); err != nil {
		result(w, nil, err, 0)
		return
	}
	value, err := s.db.AdvanceReachSession(r.PathValue("name"), r.PathValue("handle"), q)
	result(w, value, err, http.StatusOK)
}

func (s service) reachProgress(w http.ResponseWriter, r *http.Request) {
	value, err := s.db.ReachSessionProgress(r.PathValue("name"), r.PathValue("handle"))
	result(w, value, err, http.StatusOK)
}

func (s service) closeReach(w http.ResponseWriter, r *http.Request) {
	err := s.db.CloseReachSession(r.PathValue("name"), r.PathValue("handle"))
	result(w, nil, err, http.StatusNoContent)
}
