package view

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"rillweave/graph"
)

const (
	MaxHandles = 16
	// MinLease is the shortest selectable time-to-live for a finite pin.
	MinLease = time.Second
	// MaxLease bounds how long a single lease may postpone reclamation.
	MaxLease = 24 * time.Hour
)

// Lease selects how a pin retires. Zero ttl means an indefinite pin whose
// behaviour matches an explicit-release-only client. A positive ttl makes the
// handle eligible for lazy reclamation once its deadline passes.
type Lease struct {
	TTL time.Duration
}

// shared is one retained value with the handles and in-flight reads that keep
// it alive. Several handles may pin the same revision, so retiring a handle
// only releases the underlying data when this count reaches zero.
type shared[T comparable] struct {
	value T
	refs  int
}

type record[T comparable] struct {
	graph   string
	value   *shared[T]
	expires time.Time // zero value: the pin never expires on its own
}

// Handles keeps graph-scoped tokens for immutable values. The registry is safe
// for concurrent use; the read-reference release callback returned by Resolve
// may run without the caller holding any other lock. The clock is injectable
// so bounded leases can be observed without real-time sleeps.
type Handles[T comparable] struct {
	mu     sync.Mutex
	live   map[string]*record[T]
	shared map[*shared[T]]struct{}
	now    func() time.Time
}

func New[T comparable]() *Handles[T] {
	return &Handles[T]{
		live:   make(map[string]*record[T]),
		shared: make(map[*shared[T]]struct{}),
		now:    time.Now,
	}
}

// WithClock replaces the time source used for lease deadlines and expiry.
func (h *Handles[T]) WithClock(clock func() time.Time) *Handles[T] {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = clock
	return h
}

// Hold allocates a token for value under the requested lease. Expired handles
// are reclaimed first so a forgotten client cannot block a new allocation.
func (h *Handles[T]) Hold(name string, value T, lease Lease) (string, error) {
	if err := validLease(lease.TTL); err != nil {
		return "", err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reclaim()
	if len(h.live) == MaxHandles {
		return "", graph.Fail(graph.Budget, "view handle limit reached")
	}
	entry := h.retain(value)
	for {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			h.release(entry)
			return "", graph.Fail(graph.Unavailable, "cannot allocate view handle")
		}
		token := hex.EncodeToString(raw[:])
		if _, exists := h.live[token]; !exists {
			h.live[token] = &record[T]{graph: name, value: entry, expires: h.deadline(lease.TTL)}
			return token, nil
		}
	}
}

// Resolve returns the pinned value and a release callback. The callback must
// be invoked once the read using the value has finished: an expired or
// released handle is then safe to retire even while that read is in flight.
func (h *Handles[T]) Resolve(name, token string) (T, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rec, ok := h.live[token]
	if !ok || rec.graph != name || h.expired(rec) {
		var zero T
		return zero, nil, graph.Fail(graph.Missing, "view handle is not available for this graph")
	}
	rec.value.refs++
	var done bool
	release := func() {
		if done {
			return
		}
		done = true
		h.mu.Lock()
		defer h.mu.Unlock()
		h.release(rec.value)
	}
	return rec.value.value, release, nil
}

// Renew extends an existing pin. A zero ttl keeps the pin's current duration;
// a positive ttl replaces it and may also convert an indefinite pin. It
// returns the pinned value, the effective lease duration (zero for an
// indefinite pin) and the new deadline.
func (h *Handles[T]) Renew(name, token string, ttl time.Duration) (T, time.Duration, time.Time, error) {
	var zero T
	if err := validLease(ttl); err != nil {
		return zero, 0, time.Time{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	rec, ok := h.live[token]
	if !ok || rec.graph != name || h.expired(rec) {
		return zero, 0, time.Time{}, graph.Fail(graph.Missing, "view handle is not available for this graph")
	}
	effective := ttl
	switch {
	case ttl > 0:
		rec.expires = h.deadline(ttl)
	case rec.expires.IsZero():
		// An indefinite pin renewed without a ttl stays indefinite.
	default:
		// Keep the existing deadline and report what is left of the lease.
		effective = rec.expires.Sub(h.now())
	}
	return rec.value.value, effective, rec.expires, nil
}

// Release retires one handle. The pinned data is freed only when no other
// handle or resolved read still references it.
func (h *Handles[T]) Release(name, token string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	rec, ok := h.live[token]
	if !ok || rec.graph != name {
		return graph.Fail(graph.Missing, "view handle is not available for this graph")
	}
	delete(h.live, token)
	h.release(rec.value)
	return nil
}

// Deadline reports the lease deadline for a live handle and whether it is set.
func (h *Handles[T]) Deadline(token string) (time.Time, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if rec, ok := h.live[token]; ok && !h.expired(rec) {
		return rec.expires, !rec.expires.IsZero()
	}
	return time.Time{}, false
}

// LiveCount and SharedCount support bounded verification of reclamation.
func (h *Handles[T]) LiveCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.live)
}

func (h *Handles[T]) SharedCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.shared)
}

// retain finds the entry for an already retained value or creates one.
func (h *Handles[T]) retain(value T) *shared[T] {
	for entry := range h.shared {
		if entry.value == value {
			entry.refs++
			return entry
		}
	}
	entry := &shared[T]{value: value, refs: 1}
	h.shared[entry] = struct{}{}
	return entry
}

func (h *Handles[T]) release(entry *shared[T]) {
	entry.refs--
	if entry.refs <= 0 {
		delete(h.shared, entry)
	}
}

// reclaim removes handles whose lease deadline has passed. It is lazy: expiry
// only frees a new-applicant slot when some operation touches the registry.
// Callers must hold h.mu.
func (h *Handles[T]) reclaim() {
	for token, rec := range h.live {
		if h.expired(rec) {
			delete(h.live, token)
			h.release(rec.value)
		}
	}
}

func (h *Handles[T]) expired(rec *record[T]) bool {
	return !rec.expires.IsZero() && !h.now().Before(rec.expires)
}

func (h *Handles[T]) deadline(ttl time.Duration) time.Time {
	if ttl <= 0 {
		return time.Time{}
	}
	return h.now().Add(ttl)
}

func validLease(ttl time.Duration) error {
	if ttl < 0 || ttl > 0 && ttl < MinLease || ttl > MaxLease {
		return graph.Fail(graph.Invalid, "pin ttl must be between %s and %s", MinLease, MaxLease)
	}
	return nil
}
