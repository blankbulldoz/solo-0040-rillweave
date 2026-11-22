package engine

import (
	"time"

	"rillweave/change"
	"rillweave/graph"
	"rillweave/image"
	"rillweave/view"
)

func (db *Database) resolve(name, handle string) (*state, error) {
	if err := db.available(); err != nil {
		return nil, err
	}
	if handle != "" {
		s, release, err := db.held.Resolve(name, handle)
		if err != nil {
			return nil, err
		}
		// The caller holds the database lock and only needs the pointer while
		// it is held; release the read reference before returning. Callers
		// that perform an actual read use Database.read instead.
		release()
		return s, nil
	}
	if head := db.heads[name]; head != nil {
		return head, nil
	}
	return nil, graph.Fail(graph.Missing, "graph %q not found", name)
}

// read resolves a view and keeps a read reference for the lifetime of f, so a
// concurrent release or lease expiry cannot retire the data mid-query.
func (db *Database) read(name, handle string, f func(*state) error) error {
	db.mu.RLock()
	if err := db.available(); err != nil {
		db.mu.RUnlock()
		return err
	}
	if handle == "" {
		head := db.heads[name]
		db.mu.RUnlock()
		if head == nil {
			return graph.Fail(graph.Missing, "graph %q not found", name)
		}
		return f(head)
	}
	s, release, err := db.held.Resolve(name, handle)
	db.mu.RUnlock()
	if err != nil {
		return err
	}
	defer release()
	return f(s)
}

// PinLease is the optional retirement policy for a pin. A zero ttl reproduces
// the original indefinite pin that only disappears on explicit release.
type PinLease struct {
	TTL time.Duration
}

type PinResult struct {
	Handle    string     `json:"handle"`
	TTLMillis int64      `json:"ttl_millis,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Summary
}

func (db *Database) Hold(name string, lease PinLease) (PinResult, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	head, err := db.resolve(name, "")
	if err != nil {
		return PinResult{}, err
	}
	token, err := db.held.Hold(name, head, view.Lease{TTL: lease.TTL})
	if err != nil {
		return PinResult{}, err
	}
	result := PinResult{Handle: token, Summary: summary(head.graph)}
	if lease.TTL > 0 {
		deadline, _ := db.held.Deadline(token)
		result.TTLMillis = lease.TTL.Milliseconds()
		result.ExpiresAt = &deadline
	}
	return result, nil
}

func (db *Database) Renew(name, handle string, ttl time.Duration) (PinResult, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.available(); err != nil {
		return PinResult{}, err
	}
	s, effective, deadline, err := db.held.Renew(name, handle, ttl)
	if err != nil {
		return PinResult{}, err
	}
	result := PinResult{Handle: handle, Summary: summary(s.graph)}
	if effective > 0 {
		result.TTLMillis = effective.Milliseconds()
		result.ExpiresAt = &deadline
	}
	return result, nil
}

func (db *Database) Release(name, handle string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.available(); err != nil {
		return err
	}
	return db.held.Release(name, handle)
}

func (db *Database) Comparison(name, fromHandle, toHandle string) (change.Difference, error) {
	var from, to *state
	err := db.withPair(name, fromHandle, toHandle, func(a, b *state) error {
		from, to = a, b
		return nil
	})
	if err != nil {
		return change.Difference{}, err
	}
	return change.Compare(from.graph, to.graph), nil
}

// withPair resolves both views together and holds both read references until
// the comparison has finished, so neither side can switch revision or be
// reclaimed after the operation has started.
func (db *Database) withPair(name, fromHandle, toHandle string, f func(from, to *state) error) error {
	db.mu.RLock()
	if err := db.available(); err != nil {
		db.mu.RUnlock()
		return err
	}
	from, fromRelease, err := db.resolveRead(name, fromHandle)
	if err != nil {
		db.mu.RUnlock()
		return err
	}
	to, toRelease, err := db.resolveRead(name, toHandle)
	db.mu.RUnlock()
	if err != nil {
		fromRelease()
		return err
	}
	defer fromRelease()
	defer toRelease()
	return f(from, to)
}

// resolveRead mirrors resolve but retains the read reference for pinned views.
// The caller must hold db.mu and must invoke the returned release callback.
func (db *Database) resolveRead(name, handle string) (*state, func(), error) {
	if handle == "" {
		head := db.heads[name]
		if head == nil {
			return nil, nil, graph.Fail(graph.Missing, "graph %q not found", name)
		}
		return head, func() {}, nil
	}
	return db.held.Resolve(name, handle)
}

func (db *Database) Export(name, handle string) (image.Envelope, error) {
	var envelope image.Envelope
	err := db.read(name, handle, func(s *state) error {
		var encodeErr error
		envelope, encodeErr = image.Encode(s.graph)
		return encodeErr
	})
	return envelope, err
}
