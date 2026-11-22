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
		return db.held.Resolve(name, handle)
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

func (db *Database) Comparison(name string, q change.Query) (change.Difference, error) {
	if !q.Detail && (q.Fields != nil || q.Limit != 0 || q.Cursor != "") {
		return change.Difference{}, graph.Fail(graph.Invalid, "fields, limit and cursor require detail: true")
	}
	if q.Detail && q.Limit < 0 {
		return change.Difference{}, graph.Fail(graph.Invalid, "detail limit must not be negative")
	}
	from, to, err := db.pair(name, q.From, q.To)
	if err != nil {
		return change.Difference{}, err
	}
	if q.Detail {
		return change.CompareDetailed(from.graph, to.graph, change.DetailQuery{
			Fields: q.Fields, Limit: q.Limit, Cursor: q.Cursor,
		})
	}
	return change.Compare(from.graph, to.graph), nil
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
	return image.Encode(s.graph)
}
