package engine

import (
	"rillweave/change"
	"rillweave/graph"
)

// Rehearsal is the receipt for a candidate that passed the same final-state
// admission as a commit but was never published. The handle selects an
// in-memory immutable state; it is not a durable revision and disappears when
// the process exits.
type Rehearsal struct {
	Handle           string  `json:"handle"`
	Rehearsal        bool    `json:"rehearsal"`
	Published        bool    `json:"published"`
	BaselineRevision uint64  `json:"baseline_revision"`
	HeadRevision     uint64  `json:"head_revision"`
	StaleBaseline    bool    `json:"stale_baseline"`
	Candidate        Summary `json:"candidate"`
}

// Rehearse applies an ordered edit batch to a private candidate using the same
// base check, ordered application and final catalog validation as Commit.
// The baseline is the current head unless an explicit pin is given, which
// permits rehearsing an older revision after the head has advanced. Nothing is
// written to disk and the published head is left untouched.
func (db *Database) Rehearse(name string, batch change.Batch, baseline string) (Rehearsal, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.available(); err != nil {
		return Rehearsal{}, err
	}
	head := db.heads[name]
	if head == nil {
		return Rehearsal{}, graph.Fail(graph.Missing, "graph %q not found", name)
	}
	base := head
	if baseline != "" {
		if _, ok := db.rehearsed.Peek(name, baseline); ok {
			return Rehearsal{}, graph.Fail(graph.Invalid, "baseline must be a pin created with POST .../pins")
		}
		pinned, err := db.held.Resolve(name, baseline)
		if err != nil {
			return Rehearsal{}, err
		}
		base = pinned
	}
	next, err := admit(base, batch)
	if err != nil {
		return Rehearsal{}, err
	}
	candidate := materialize(next)
	candidate.rehearsal = true
	candidate.baseline = base.graph.Revision
	token, err := db.rehearsed.Hold(name, candidate)
	if err != nil {
		return Rehearsal{}, err
	}
	return Rehearsal{
		Handle: token, Rehearsal: true, Published: false,
		BaselineRevision: base.graph.Revision, HeadRevision: head.graph.Revision,
		StaleBaseline: head.graph.Revision != base.graph.Revision, Candidate: summary(next),
	}, nil
}

// Discard drops a rehearsal candidate. Rehearsal handles and durable pins use
// separate pools, so discarding one never releases the other.
func (db *Database) Discard(name, handle string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.available(); err != nil {
		return err
	}
	return db.rehearsed.Release(name, handle)
}
