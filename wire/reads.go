package wire

import (
	"net/http"
	"strings"

	"rillweave/engine"
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
	cacheable(w, r, value, err)
}

func (s service) vertex(w http.ResponseWriter, r *http.Request) {
	value, err := s.db.Vertex(r.PathValue("name"), selected(r), r.PathValue("id"))
	cacheable(w, r, value, err)
}

// cacheable answers a read whose response carries a validator. The client's
// credential only shortens a successful response: resolution and resource
// errors are always reported with their own status, never as a reuse notice.
func cacheable[T any](w http.ResponseWriter, r *http.Request, value engine.ReadResult[T], err error) {
	if err != nil {
		result(w, value, err, http.StatusOK)
		return
	}
	etag := `"` + value.Validator + `"`
	w.Header().Set("ETag", etag)
	if matches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	result(w, value, nil, http.StatusOK)
}

// matches reports whether an If-None-Match header authorizes reuse of the
// representation carrying etag. Comparison is weak (RFC 7232); "*" matches
// any current representation.
func matches(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		candidate := strings.TrimPrefix(strings.TrimSpace(part), "W/")
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
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
