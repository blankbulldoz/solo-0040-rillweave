package engine

import (
	"crypto/rand"
	"encoding/hex"
	"sync"

	"rillweave/graph"
	"rillweave/index"
)

// MaxContinuations bounds process-local ordered lookup cursors. A cursor holds
// the immutable state and revision it was opened against, not a writable head,
// and is dropped when its range is exhausted, on eviction or on restart.
const MaxContinuations = 64

type cursor struct {
	graph    string
	view     string
	state    *state
	kind     string
	property string
	bound    graph.Range
	pos      index.Position
	clock    uint64
}

type continuations struct {
	mu    sync.Mutex
	live  map[string]*cursor
	clock uint64
}

func newContinuations() *continuations {
	return &continuations{live: make(map[string]*cursor)}
}

func (c *continuations) open(record cursor) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mint(&record)
}

// mint stores record under a fresh token and returns it. The registry lock must
// be held. Tokens are single-use: resume deletes the token it consumed, so
// replaying an old token is reported as expired rather than skipping a page.
func (c *continuations) mint(record *cursor) (string, error) {
	if len(c.live) == MaxContinuations {
		c.evictOldest()
	}
	for {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", graph.Fail(graph.Unavailable, "cannot allocate lookup continuation")
		}
		token := hex.EncodeToString(raw[:])
		if _, exists := c.live[token]; exists {
			continue
		}
		c.clock++
		record.clock = c.clock
		c.live[token] = record
		return token, nil
	}
}

// evictOldest drops the least recently resumed cursor. A caller of an evicted
// cursor receives an explicit restart error rather than a silently
// discontinuous page.
func (c *continuations) evictOldest() {
	var oldest string
	for token, record := range c.live {
		if oldest == "" || record.clock < c.live[oldest].clock {
			oldest = token
		}
	}
	delete(c.live, oldest)
}

// page is one resumed window plus the snapshot metadata the client needs to
// keep reading without guessing which revision or range it is on.
type page struct {
	window   index.Page
	revision uint64
	kind     string
	property string
	bound    graph.Range
	next     string
}

// resume advances one cursor page while the registry lock is held, so two
// concurrent pages cannot both resume from the same token. The scan reads the
// bound state directly; later commits publish a new state and cannot alter this
// snapshot. handle must repeat the view selector the cursor was opened with:
// a head cursor stays on its revision even after the head advances, and a
// pinned cursor stays on that pin. The consumed token is always retired; a
// further page is issued under a new token only when unread entries remain.
func (c *continuations) resume(name, handle string, q LookupRequest, limit int) (page, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	record, ok := c.live[q.After]
	if !ok {
		return page{}, graph.Fail(graph.Missing, "lookup continuation has expired; restart the lookup without a continuation")
	}
	if record.graph != name {
		return page{}, graph.Fail(graph.Missing, "lookup continuation is not available for this graph")
	}
	if handle != record.view {
		return page{}, graph.Fail(graph.Invalid, "lookup continuation is bound to a different view; restart the lookup")
	}
	if err := echoMatches(record, q); err != nil {
		return page{}, err
	}
	window, err := record.state.props.Scan(record.kind, record.property, record.bound, &record.pos, limit)
	if err != nil {
		delete(c.live, q.After)
		return page{}, graph.Fail(graph.Invalid, "lookup continuation could not resume: %v", err)
	}
	delete(c.live, q.After)
	next := ""
	if window.Truncated && window.Last != nil {
		advanced := *record
		advanced.pos = *window.Last
		next, err = c.mint(&advanced)
		if err != nil {
			return page{}, err
		}
	}
	return page{
		window: window, revision: record.state.graph.Revision,
		kind: record.kind, property: record.property, bound: record.bound, next: next,
	}, nil
}

// discardView drops cursors opened against one pinned view, so releasing the
// handle also ends its continuations instead of leaving unbounded reads alive.
func (c *continuations) discardView(name, view string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for token, record := range c.live {
		if record.graph == name && record.view == view {
			delete(c.live, token)
		}
	}
}

// echoMatches lets a client repeat the cursor's query, but a repeated kind,
// property or range must be the one the snapshot was opened with.
func echoMatches(record *cursor, q LookupRequest) error {
	if q.Value != nil {
		return graph.Fail(graph.Invalid, "a continuation cannot carry an equality value; restart the lookup")
	}
	if q.Kind != "" && q.Kind != record.kind {
		return graph.Fail(graph.Invalid, "lookup continuation belongs to another vertex kind; restart the lookup")
	}
	if q.Property != "" && q.Property != record.property {
		return graph.Fail(graph.Invalid, "lookup continuation belongs to another property; restart the lookup")
	}
	if q.Range != nil && !sameRange(*q.Range, record.bound) {
		return graph.Fail(graph.Invalid, "lookup continuation belongs to another range; restart the lookup")
	}
	return nil
}

func sameRange(a, b graph.Range) bool {
	return sameBound(a.From, b.From) && sameBound(a.To, b.To)
}

func sameBound(a, b *graph.Bound) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Exclusive == b.Exclusive && graph.Compare(a.Value, b.Value) == 0
}
