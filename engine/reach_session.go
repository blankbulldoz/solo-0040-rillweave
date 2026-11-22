package engine

import (
	"crypto/rand"
	"encoding/hex"

	"rillweave/graph"
	"rillweave/walk"
)

// MaxReachSessions bounds server-side resumable traversals, matching the
// number of simultaneous pinned views.
const MaxReachSessions = 16

// reachSession pins one immutable read state for the whole traversal, so writes
// committed between pages cannot change the answer. All mutable cursor state
// stays on the server; the client holds only the opaque handle.
type reachSession struct {
	graph    string
	revision uint64
	state    *state
	cursor   *walk.ReachCursor
	done     bool
}

type reachSessions struct {
	live map[string]*reachSession
}

func newReachSessions() reachSessions {
	return reachSessions{live: make(map[string]*reachSession)}
}

func (r reachSessions) token() (string, error) {
	for {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", graph.Fail(graph.Unavailable, "cannot allocate reach session handle")
		}
		token := hex.EncodeToString(raw[:])
		if _, exists := r.live[token]; !exists {
			return token, nil
		}
	}
}

func (r reachSessions) resolve(name, handle string) (*reachSession, error) {
	if session, ok := r.live[handle]; ok && session.graph == name {
		return session, nil
	}
	return nil, graph.Fail(graph.Missing, "reach session is not available for this graph")
}

type ReachOpenResult struct {
	Handle   string         `json:"handle"`
	Revision uint64         `json:"revision"`
	Page     walk.ReachPage `json:"page"`
}

type ReachProgress struct {
	Revision uint64 `json:"revision"`
	Scanned  int    `json:"scanned"`
	Complete bool   `json:"complete"`
}

// OpenReachSession pins the selected state and returns the first page, which
// always contains the root at zero hops.
func (db *Database) OpenReachSession(name, handle string, q walk.ReachPageRequest) (ReadResult[ReachOpenResult], error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	s, err := db.resolve(name, handle)
	if err != nil {
		return ReadResult[ReachOpenResult]{}, err
	}
	cursor, err := walk.NewReachCursor(s.graph, s.adj, q)
	if err != nil {
		return ReadResult[ReachOpenResult]{}, err
	}
	// Registry mutation is serialized with advances and closes; the lock
	// order is db.mu before reachMu, and reachMu is never held back into
	// db.mu.
	db.reachMu.Lock()
	defer db.reachMu.Unlock()
	if len(db.reach.live) == MaxReachSessions {
		return ReadResult[ReachOpenResult]{}, graph.Fail(graph.Budget, "reach session limit reached")
	}
	token, err := db.reach.token()
	if err != nil {
		return ReadResult[ReachOpenResult]{}, err
	}
	session := &reachSession{
		graph: name, revision: s.graph.Revision, state: s, cursor: cursor,
	}
	db.reach.live[token] = session
	page, err := cursor.Advance(s.graph, s.adj, q.Budget)
	if err != nil {
		delete(db.reach.live, token)
		return ReadResult[ReachOpenResult]{}, err
	}
	session.done = page.Complete
	return ReadResult[ReachOpenResult]{
		Graph: name, Revision: s.graph.Revision,
		Result: ReachOpenResult{Handle: token, Revision: s.graph.Revision, Page: page},
	}, nil
}

// AdvanceReachSession continues a pinned traversal by one bounded page. A
// missing or released handle is distinct from a completed frontier: the former
// is not_found, the latter returns complete:true.
func (db *Database) AdvanceReachSession(name, handle string, q walk.ReachContinue) (ReadResult[walk.ReachPage], error) {
	db.reachMu.Lock()
	defer db.reachMu.Unlock()
	session, err := db.reach.resolve(name, handle)
	if err != nil {
		return ReadResult[walk.ReachPage]{}, err
	}
	page, err := session.cursor.Advance(session.state.graph, session.state.adj, q.Budget)
	if err != nil {
		return ReadResult[walk.ReachPage]{}, err
	}
	session.done = page.Complete
	return ReadResult[walk.ReachPage]{
		Graph: session.graph, Revision: session.revision, Result: page,
	}, nil
}

// ReachSessionProgress reports continuation state without consuming budget.
func (db *Database) ReachSessionProgress(name, handle string) (ReadResult[ReachProgress], error) {
	db.reachMu.RLock()
	defer db.reachMu.RUnlock()
	session, err := db.reach.resolve(name, handle)
	if err != nil {
		return ReadResult[ReachProgress]{}, err
	}
	return ReadResult[ReachProgress]{
		Graph: session.graph, Revision: session.revision,
		Result: ReachProgress{
			Revision: session.revision,
			Scanned:  session.cursor.Scanned(),
			Complete: session.done,
		},
	}, nil
}

// CloseReachSession releases the pinned traversal state.
func (db *Database) CloseReachSession(name, handle string) error {
	db.reachMu.Lock()
	defer db.reachMu.Unlock()
	if _, err := db.reach.resolve(name, handle); err != nil {
		return err
	}
	delete(db.reach.live, handle)
	return nil
}
