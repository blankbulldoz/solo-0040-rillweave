package engine

import (
	"sync"

	"rillweave/graph"
	"rillweave/walk"
)

// MaxPathSessions bounds how many paused path searches one process retains.
// Sessions are in-memory only and never survive a restart.
const MaxPathSessions = 16

// pathSession pins one immutable read state together with its resumable
// search. A later commit replaces the head pointer but never the state held
// here, so every candidate discovered earlier stays valid for continuation.
type pathSession struct {
	mu     sync.Mutex
	search *walk.PathSearch
	state  *state
}

// PathOutcome is the wire view of one search segment. Session is present only
// while the search is paused; a completed segment closes the session.
type PathOutcome struct {
	walk.PathResult
	Session string `json:"session,omitempty"`
}

// Path starts or resumes a bounded path search. A new request runs one
// allowance-bounded segment and, when paused, returns a session handle. A
// session request must repeat the original query and may pin the same view.
func (db *Database) Path(name, handle string, q walk.PathRequest, token string) (ReadResult[PathOutcome], error) {
	if token != "" {
		return db.resumePath(name, handle, q, token)
	}
	return db.startPath(name, handle, q)
}

func (db *Database) startPath(name, handle string, q walk.PathRequest) (ReadResult[PathOutcome], error) {
	db.mu.RLock()
	s, err := db.resolve(name, handle)
	db.mu.RUnlock()
	if err != nil {
		return ReadResult[PathOutcome]{}, err
	}
	search, err := walk.NewPathSearch(s.graph, s.adj, q)
	if err != nil {
		return ReadResult[PathOutcome]{}, err
	}
	result, err := search.Advance(q.Budget)
	if err != nil {
		return ReadResult[PathOutcome]{}, err
	}
	outcome := PathOutcome{PathResult: result}
	if !result.Complete {
		db.mu.Lock()
		if err := db.available(); err != nil {
			db.mu.Unlock()
			return ReadResult[PathOutcome]{}, err
		}
		session, holdErr := db.pathSessions.Hold(name, &pathSession{search: search, state: s})
		db.mu.Unlock()
		if holdErr != nil {
			return ReadResult[PathOutcome]{}, holdErr
		}
		outcome.Session = session
	}
	return ReadResult[PathOutcome]{
		Graph: s.graph.Name, Revision: s.graph.Revision, Result: outcome,
	}, nil
}

func (db *Database) resumePath(name, handle string, q walk.PathRequest, token string) (ReadResult[PathOutcome], error) {
	db.mu.RLock()
	if err := db.available(); err != nil {
		db.mu.RUnlock()
		return ReadResult[PathOutcome]{}, err
	}
	record, err := db.pathSessions.Resolve(name, token)
	if err != nil {
		db.mu.RUnlock()
		return ReadResult[PathOutcome]{}, err
	}
	// An explicit view must name the same immutable state the session pinned.
	if handle != "" {
		pinned, viewErr := db.held.Resolve(name, handle)
		if viewErr != nil {
			db.mu.RUnlock()
			return ReadResult[PathOutcome]{}, viewErr
		}
		if pinned != record.state {
			db.mu.RUnlock()
			return ReadResult[PathOutcome]{}, graph.Fail(graph.Conflict, "selected view does not match the path session")
		}
	}
	db.mu.RUnlock()

	record.mu.Lock()
	if !record.search.Agreement(q) {
		record.mu.Unlock()
		return ReadResult[PathOutcome]{}, graph.Fail(
			graph.Conflict,
			"continuation must repeat the original from, to, max_hops, direction and arc kinds",
		)
	}
	result, err := record.search.Advance(q.Budget)
	record.mu.Unlock()
	if err != nil {
		return ReadResult[PathOutcome]{}, err
	}
	outcome := PathOutcome{PathResult: result}
	if result.Complete {
		// Best-effort close: an explicit release may have won the race, in
		// which case the token is already gone and nothing else is needed.
		db.mu.Lock()
		_ = db.pathSessions.Release(name, token)
		db.mu.Unlock()
	} else {
		outcome.Session = token
	}
	return ReadResult[PathOutcome]{
		Graph:    record.state.graph.Name,
		Revision: record.state.graph.Revision,
		Result:   outcome,
	}, nil
}

// ReleasePath discards a paused session. Its handle never resolves again, so a
// released search cannot be mixed with any later, different path.
func (db *Database) ReleasePath(name, token string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.available(); err != nil {
		return err
	}
	return db.pathSessions.Release(name, token)
}
