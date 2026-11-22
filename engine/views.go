package engine

import (
	"rillweave/change"
	"rillweave/graph"
	"rillweave/image"
)

func (db *Database) resolve(name, handle string) (*state, error) {
	if err := db.available(); err != nil {
		return nil, err
	}
	if handle != "" {
		if s, ok := db.held.Peek(name, handle); ok {
			return s, nil
		}
		if s, ok := db.rehearsed.Peek(name, handle); ok {
			return s, nil
		}
		return nil, graph.Fail(graph.Missing, "view handle is not available for this graph")
	}
	if head := db.heads[name]; head != nil {
		return head, nil
	}
	return nil, graph.Fail(graph.Missing, "graph %q not found", name)
}

func (db *Database) read(name, handle string) (*state, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.resolve(name, handle)
}

type PinResult struct {
	Handle string `json:"handle"`
	Summary
}

func (db *Database) Hold(name string) (PinResult, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	head, err := db.resolve(name, "")
	if err != nil {
		return PinResult{}, err
	}
	token, err := db.held.Hold(name, head)
	if err != nil {
		return PinResult{}, err
	}
	return PinResult{token, summary(head.graph)}, nil
}

func (db *Database) Release(name, handle string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.available(); err != nil {
		return err
	}
	return db.held.Release(name, handle)
}

func (db *Database) Comparison(name, fromHandle, toHandle string) (ComparisonResult, error) {
	from, to, err := db.pair(name, fromHandle, toHandle)
	if err != nil {
		return ComparisonResult{}, err
	}
	out := ComparisonResult{Difference: change.Compare(from.graph, to.graph)}
	// A difference involving an unpublished candidate is labelled as such, even
	// when the other side is the durable head.
	if from.rehearsal || to.rehearsal {
		no := false
		out.Rehearsal, out.Published = true, &no
	}
	return out, nil
}

type ComparisonResult struct {
	change.Difference
	Rehearsal bool  `json:"rehearsal,omitempty"`
	Published *bool `json:"published,omitempty"`
}

func (db *Database) pair(name, fromHandle, toHandle string) (*state, *state, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	from, err := db.resolve(name, fromHandle)
	if err != nil {
		return nil, nil, err
	}
	to, err := db.resolve(name, toHandle)
	return from, to, err
}

func (db *Database) Export(name, handle string) (image.Envelope, error) {
	s, err := db.read(name, handle)
	if err != nil {
		return image.Envelope{}, err
	}
	if s.rehearsal {
		return image.Envelope{}, graph.Fail(graph.Invalid,
			"a rehearsal candidate has no publishable image; hold a pin or commit it first")
	}
	return image.Encode(s.graph)
}

// identity returns the revision and headers a rehearsal candidate carries, so
// wire responses can never present an unpublished state as a durable head.
func identity(s *state) (rehearsal bool, published *bool, baseline uint64) {
	if !s.rehearsal {
		return false, nil, 0
	}
	no := false
	return true, &no, s.baseline
}
