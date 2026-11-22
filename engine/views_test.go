package engine

import (
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"rillweave/change"
	"rillweave/graph"
	"rillweave/view"
)

// changeCommit publishes one empty schema change to advance the head.
func changeCommit(db *Database) (uint64, error) {
	head, err := db.Catalog("abutment", "")
	if err != nil {
		return 0, err
	}
	summary, err := db.Commit("abutment", change.Batch{
		Base: head.Revision,
		Edits: []change.Edit{{
			Op: "put_vertex",
			Vertex: &graph.Vertex{
				ID: "v" + strconv.Itoa(int(head.Revision)), Kind: "strut",
			},
		}},
	})
	if err != nil {
		return 0, err
	}
	return summary.Revision, nil
}

type testClock struct{ t time.Time }

func (c *testClock) now() time.Time          { return c.t }
func (c *testClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func openTestDatabase(t *testing.T) (*Database, *testClock) {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clock := &testClock{t: time.Unix(1_000_000, 0)}
	db.held.WithClock(clock.now)
	if _, err := db.Create("abutment", testSchema()); err != nil {
		t.Fatalf("create: %v", err)
	}
	return db, clock
}

func testSchema() graph.Schema {
	return graph.Schema{Vertices: map[string]graph.VertexRule{"strut": {}}}
}

func engineCode(err error) graph.Code {
	var problem *graph.Problem
	if errors.As(err, &problem) {
		return problem.Code
	}
	return ""
}

func TestHoldLeaseExpiryStopsResolving(t *testing.T) {
	db, clock := openTestDatabase(t)
	pin, err := db.Hold("abutment", PinLease{TTL: 2 * time.Second})
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if pin.TTLMillis != 2000 || pin.ExpiresAt == nil || pin.ExpiresAt.IsZero() || pin.Revision != 1 {
		t.Fatalf("unexpected receipt: %+v", pin)
	}
	if _, err := db.Catalog("abutment", pin.Handle); err != nil {
		t.Fatalf("pinned read before expiry: %v", err)
	}
	clock.advance(2 * time.Second)
	if _, err := db.Catalog("abutment", pin.Handle); engineCode(err) != graph.Missing {
		t.Fatalf("expected missing after lease expiry, got %v", err)
	}
	// Expired slot is reclaimed and a new pin can be allocated up to the bound.
	if _, err := db.Hold("abutment", PinLease{}); err != nil {
		t.Fatalf("reallocation after expiry: %v", err)
	}
}

func TestIndefinitePinBehavesAsBefore(t *testing.T) {
	db, clock := openTestDatabase(t)
	pin, err := db.Hold("abutment", PinLease{})
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if pin.TTLMillis != 0 || pin.ExpiresAt != nil {
		t.Fatalf("indefinite pin carries lease fields: %+v", pin)
	}
	clock.advance(view.MaxLease * 2)
	if _, err := db.Catalog("abutment", pin.Handle); err != nil {
		t.Fatalf("indefinite pin expired: %v", err)
	}
	if err := db.Release("abutment", pin.Handle); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := db.Catalog("abutment", pin.Handle); engineCode(err) != graph.Missing {
		t.Fatalf("released pin still resolves: %v", err)
	}
}

func TestRenewalKeepsPinAlive(t *testing.T) {
	db, clock := openTestDatabase(t)
	pin, err := db.Hold("abutment", PinLease{TTL: 3 * time.Second})
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	clock.advance(2 * time.Second)
	receipt, err := db.Renew("abutment", pin.Handle, 3*time.Second)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if receipt.TTLMillis != 3000 {
		t.Fatalf("renewal receipt ttl = %d", receipt.TTLMillis)
	}
	clock.advance(2 * time.Second) // 4s overall: original lease would have lapsed.
	if _, err := db.Catalog("abutment", pin.Handle); err != nil {
		t.Fatalf("renewed pin expired early: %v", err)
	}
	if _, err := db.Renew("abutment", "unknown", time.Second); engineCode(err) != graph.Missing {
		t.Fatalf("renew unknown handle: %v", err)
	}
	if _, err := db.Renew("abutment", pin.Handle, time.Millisecond); engineCode(err) != graph.Invalid {
		t.Fatalf("renew with sub-second ttl: %v", err)
	}
}

func TestPinnedReadDoesNotSwitchRevision(t *testing.T) {
	db, _ := openTestDatabase(t)
	pin, err := db.Hold("abutment", PinLease{})
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	next, err := changeCommit(db)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	head, err := db.Catalog("abutment", "")
	if err != nil || head.Revision != next {
		t.Fatalf("head read: rev=%d err=%v", next, err)
	}
	fixed, err := db.Catalog("abutment", pin.Handle)
	if err != nil || fixed.Revision != 1 {
		t.Fatalf("pinned read switched revision: %+v err=%v", fixed, err)
	}
	// Comparison keeps both sides on their resolved revisions.
	diff, err := db.Comparison("abutment", pin.Handle, "")
	if err != nil || diff.From != 1 || diff.To != next {
		t.Fatalf("comparison: %+v err=%v", diff, err)
	}
}

func TestInFlightReadSurvivesConcurrentRelease(t *testing.T) {
	db, _ := openTestDatabase(t)
	pin, err := db.Hold("abutment", PinLease{})
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	started := make(chan struct{})
	proceed := make(chan struct{})
	var readErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		readErr = db.read("abutment", pin.Handle, func(s *state) error {
			close(started)
			<-proceed
			if s.graph.Revision != 1 {
				t.Errorf("in-flight read saw revision %d", s.graph.Revision)
			}
			return nil
		})
	}()
	<-started
	// The read has resolved its view and released the database lock; retiring
	// the handle now must not reclaim the data underneath it.
	if err := db.Release("abutment", pin.Handle); err != nil {
		t.Fatalf("release during read: %v", err)
	}
	if db.held.SharedCount() != 1 {
		t.Fatalf("data reclaimed during in-flight read: shared=%d", db.held.SharedCount())
	}
	close(proceed)
	wg.Wait()
	if readErr != nil {
		t.Fatalf("in-flight read failed: %v", readErr)
	}
	if db.held.SharedCount() != 0 {
		t.Fatalf("data leaked after read finished: shared=%d", db.held.SharedCount())
	}
	if _, err := db.Catalog("abutment", pin.Handle); engineCode(err) != graph.Missing {
		t.Fatalf("released handle still resolves: %v", err)
	}
}

func TestSharedRevisionRetirement(t *testing.T) {
	db, clock := openTestDatabase(t)
	first, err := db.Hold("abutment", PinLease{TTL: time.Second})
	if err != nil {
		t.Fatalf("hold first: %v", err)
	}
	second, err := db.Hold("abutment", PinLease{})
	if err != nil {
		t.Fatalf("hold second: %v", err)
	}
	if db.held.SharedCount() != 1 {
		t.Fatalf("two pins on one revision should share data, got %d", db.held.SharedCount())
	}
	clock.advance(2 * time.Second)
	// Touching the registry reclaims the expired handle but keeps the data.
	if _, err := db.Hold("abutment", PinLease{}); err != nil {
		t.Fatalf("reclaim hold: %v", err)
	}
	if _, err := db.Catalog("abutment", second.Handle); err != nil {
		t.Fatalf("sibling pin lost when another handle expired: %v", err)
	}
	if _, err := db.Catalog("abutment", first.Handle); engineCode(err) != graph.Missing {
		t.Fatalf("expired handle still resolves: %v", err)
	}
	if err := db.Release("abutment", second.Handle); err != nil {
		t.Fatalf("release sibling: %v", err)
	}
}

func TestComparisonRejectsExpiredAndForeignHandles(t *testing.T) {
	db, clock := openTestDatabase(t)
	pin, err := db.Hold("abutment", PinLease{TTL: time.Second})
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if _, err := db.Create("pier", testSchema()); err != nil {
		t.Fatalf("create pier: %v", err)
	}
	foreign, err := db.Hold("pier", PinLease{})
	if err != nil {
		t.Fatalf("foreign hold: %v", err)
	}
	if _, err := db.Comparison("abutment", foreign.Handle, ""); engineCode(err) != graph.Missing {
		t.Fatalf("foreign handle in comparison: %v", err)
	}
	clock.advance(2 * time.Second)
	if _, err := db.Comparison("abutment", pin.Handle, ""); engineCode(err) != graph.Missing {
		t.Fatalf("expired handle in comparison: %v", err)
	}
}
