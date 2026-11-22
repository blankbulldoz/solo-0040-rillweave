package view

import (
	"errors"
	"sync"
	"testing"
	"time"

	"rillweave/graph"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newHandles(t *testing.T) (*Handles[int], *fakeClock) {
	t.Helper()
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	return New[int]().WithClock(clock.now), clock
}

func problemCode(err error) graph.Code {
	var problem *graph.Problem
	if errors.As(err, &problem) {
		return problem.Code
	}
	return ""
}

func TestIndefinitePinNeverExpires(t *testing.T) {
	h, clock := newHandles(t)
	token, err := h.Hold("g", 1, Lease{})
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	clock.advance(MaxLease + time.Hour)
	if _, _, err := h.Resolve("g", token); err != nil {
		t.Fatalf("indefinite pin expired: %v", err)
	}
	if deadline, ok := h.Deadline(token); ok || !deadline.IsZero() {
		t.Fatalf("indefinite pin reported a deadline: %v %v", deadline, ok)
	}
}

func TestFiniteLeaseExpiresAndFreesSlotLazily(t *testing.T) {
	h, clock := newHandles(t)
	first, err := h.Hold("g", 1, Lease{TTL: 2 * time.Second})
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	// Fill the remaining slots with indefinite pins.
	for i := 1; i < MaxHandles; i++ {
		if _, err := h.Hold("g", i, Lease{}); err != nil {
			t.Fatalf("hold %d: %v", i, err)
		}
	}
	if _, err := h.Hold("g", 99, Lease{}); problemCode(err) != graph.Budget {
		t.Fatalf("expected quota error, got %v", err)
	}
	// Before the deadline, renewal is still accepted and nothing is reclaimed.
	if _, _, _, err := h.Renew("g", first, time.Second); err != nil {
		t.Fatalf("renew before expiry: %v", err)
	}
	clock.advance(time.Second + MinLease)
	// Expired handle no longer resolves or renews.
	if _, _, err := h.Resolve("g", first); problemCode(err) != graph.Missing {
		t.Fatalf("expected missing after expiry, got %v", err)
	}
	if _, _, _, err := h.Renew("g", first, time.Second); problemCode(err) != graph.Missing {
		t.Fatalf("expected missing when renewing expired pin, got %v", err)
	}
	// The expired pin still occupies a registry slot until an operation
	// reclaims it; the next Hold must succeed by reclaiming that slot.
	replacement, err := h.Hold("g", 99, Lease{})
	if err != nil {
		t.Fatalf("hold after expiry should reclaim slot: %v", err)
	}
	if h.LiveCount() != MaxHandles {
		t.Fatalf("live count = %d, want %d", h.LiveCount(), MaxHandles)
	}
	if _, _, err := h.Resolve("g", replacement); err != nil {
		t.Fatalf("replacement pin unusable: %v", err)
	}
}

func TestRenewalExtendsDeadline(t *testing.T) {
	h, clock := newHandles(t)
	token, err := h.Hold("g", 7, Lease{TTL: 5 * time.Second})
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	clock.advance(4 * time.Second)
	if _, _, _, err := h.Renew("g", token, 5*time.Second); err != nil {
		t.Fatalf("renew: %v", err)
	}
	clock.advance(4 * time.Second) // 8s elapsed overall, but the renewal reset the deadline.
	if _, _, err := h.Resolve("g", token); err != nil {
		t.Fatalf("renewed pin should still resolve: %v", err)
	}
	clock.advance(2 * time.Second) // now past the renewed deadline
	if _, _, err := h.Resolve("g", token); problemCode(err) != graph.Missing {
		t.Fatalf("expected expiry after renewed deadline, got %v", err)
	}
}

func TestRenewWithoutTTLKeepsRemainingLease(t *testing.T) {
	h, clock := newHandles(t)
	token, err := h.Hold("g", 7, Lease{TTL: 5 * time.Second})
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	clock.advance(2 * time.Second)
	_, effective, deadline, err := h.Renew("g", token, 0)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if effective != 3*time.Second {
		t.Fatalf("effective ttl = %v, want 3s", effective)
	}
	if want := clock.now().Add(3 * time.Second); !deadline.Equal(want) {
		t.Fatalf("deadline = %v, want %v", deadline, want)
	}
	// A zero-ttl renewal of an indefinite pin must stay indefinite.
	forever, err := h.Hold("g", 8, Lease{})
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	_, effective, deadline, err = h.Renew("g", forever, 0)
	if err != nil || effective != 0 || !deadline.IsZero() {
		t.Fatalf("indefinite renewal changed lease: %v %v %v", effective, deadline, err)
	}
}

func TestInvalidTTLRejected(t *testing.T) {
	h, _ := newHandles(t)
	for _, ttl := range []time.Duration{-time.Second, time.Millisecond, MaxLease + time.Nanosecond} {
		if _, err := h.Hold("g", 1, Lease{TTL: ttl}); problemCode(err) != graph.Invalid {
			t.Fatalf("ttl %v: expected invalid, got %v", ttl, err)
		}
	}
}

func TestSharedRevisionReferenceCounting(t *testing.T) {
	h, clock := newHandles(t)
	const revision = 42
	first, err := h.Hold("g", revision, Lease{TTL: time.Second})
	if err != nil {
		t.Fatalf("hold first: %v", err)
	}
	second, err := h.Hold("g", revision, Lease{})
	if err != nil {
		t.Fatalf("hold second: %v", err)
	}
	if h.SharedCount() != 1 {
		t.Fatalf("shared entries = %d, want one shared revision", h.SharedCount())
	}
	// Expire and reclaim the first handle: the data must survive because the
	// second handle still pins it.
	clock.advance(2 * time.Second)
	replacement, err := h.Hold("g", 7, Lease{})
	if err != nil {
		t.Fatalf("reclaim hold: %v", err)
	}
	if h.SharedCount() != 2 {
		t.Fatalf("shared entries = %d, want revision 42 plus new value", h.SharedCount())
	}
	if value, releaseRead, err := h.Resolve("g", second); err != nil || value != revision {
		t.Fatalf("shared revision lost on sibling expiry: value=%d err=%v", value, err)
	} else {
		releaseRead()
	}
	// Explicit release of the remaining handle finally drops the shared data.
	if err := h.Release("g", replacement); err != nil {
		t.Fatalf("release replacement: %v", err)
	}
	if h.SharedCount() != 1 {
		t.Fatalf("shared entries = %d, want only the shared revision", h.SharedCount())
	}
	if err := h.Release("g", second); err != nil {
		t.Fatalf("release: %v", err)
	}
	if h.SharedCount() != 0 {
		t.Fatalf("shared entries = %d, want zero", h.SharedCount())
	}
	if err := h.Release("g", first); problemCode(err) != graph.Missing {
		t.Fatalf("releasing an expired handle should be missing, got %v", err)
	}
}
func TestInFlightReadSurvivesReleaseAndExpiry(t *testing.T) {
	h, clock := newHandles(t)
	token, err := h.Hold("g", 5, Lease{TTL: time.Second})
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	value, release, err := h.Resolve("g", token)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if value != 5 {
		t.Fatalf("value = %d, want 5", value)
	}
	// Both retirement paths happen while the read reference is outstanding.
	if err := h.Release("g", token); err != nil {
		t.Fatalf("release: %v", err)
	}
	clock.advance(2 * time.Second)
	if h.SharedCount() != 1 {
		t.Fatalf("data retired while a read was in flight: shared=%d", h.SharedCount())
	}
	// The released token cannot start a new read.
	if _, _, err := h.Resolve("g", token); problemCode(err) != graph.Missing {
		t.Fatalf("expected missing after release, got %v", err)
	}
	release()
	if h.SharedCount() != 0 {
		t.Fatalf("data retained after the in-flight read finished: shared=%d", h.SharedCount())
	}
}

func TestGraphScopedAndUnknownTokens(t *testing.T) {
	h, _ := newHandles(t)
	token, err := h.Hold("alpha", 1, Lease{})
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if _, _, err := h.Resolve("beta", token); problemCode(err) != graph.Missing {
		t.Fatalf("foreign resolve: %v", err)
	}
	if err := h.Release("beta", token); problemCode(err) != graph.Missing {
		t.Fatalf("foreign release: %v", err)
	}
	if _, _, _, err := h.Renew("beta", token, time.Second); problemCode(err) != graph.Missing {
		t.Fatalf("foreign renew: %v", err)
	}
	if _, _, err := h.Resolve("alpha", "deadbeef"); problemCode(err) != graph.Missing {
		t.Fatalf("unknown token: %v", err)
	}
}

// TestConcurrentRenewAndResolve keeps every pin alive and hammers renewals and
// resolved reads in parallel to prove the registry stays consistent under
// concurrent lease traffic and never reclaims a value with an active reader.
func TestConcurrentRenewAndResolve(t *testing.T) {
	h, clock := newHandles(t)
	tokens := make([]string, MaxHandles)
	for i := range tokens {
		token, err := h.Hold("g", i%3, Lease{TTL: time.Minute}) // shared revisions
		if err != nil {
			t.Fatalf("hold %d: %v", i, err)
		}
		tokens[i] = token
	}
	const workers = 8
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for round := 0; round < 200; round++ {
				for _, token := range tokens {
					if _, _, _, err := h.Renew("g", token, 90*time.Second); err != nil {
						t.Errorf("renew: %v", err)
					}
				}
			}
		}()
		go func() {
			defer wg.Done()
			for round := 0; round < 200; round++ {
				for _, token := range tokens {
					value, release, err := h.Resolve("g", token)
					if err != nil {
						t.Errorf("resolve: %v", err)
						continue
					}
					if value < 0 || value > 2 {
						t.Errorf("read corrupted value %d", value)
					}
					release()
				}
			}
		}()
	}
	wg.Wait()
	if h.LiveCount() != MaxHandles {
		t.Fatalf("live count = %d, want %d", h.LiveCount(), MaxHandles)
	}
	for _, token := range tokens {
		if _, release, err := h.Resolve("g", token); err != nil {
			t.Fatalf("surviving token unreadable: %v", err)
		} else {
			release()
		}
	}
	// Advancing past every renewed deadline must eventually free all slots,
	// proving expiry does not leave entries pinned forever.
	clock.advance(2 * time.Minute)
	if _, err := h.Hold("g", 1, Lease{}); err != nil {
		t.Fatalf("hold after batch expiry: %v", err)
	}
	if h.LiveCount() != 1 {
		t.Fatalf("expired handles were not reclaimed: live=%d", h.LiveCount())
	}
}

// TestConcurrentReleaseWithReaders interleaves explicit releases with reads:
// a read that resolved first must keep its data valid until it finishes.
func TestConcurrentReleaseWithReaders(t *testing.T) {
	h, _ := newHandles(t)
	var wg sync.WaitGroup
	for round := 0; round < 25; round++ {
		tokens := make([]string, MaxHandles)
		for i := range tokens {
			token, err := h.Hold("g", round*MaxHandles+i, Lease{})
			if err != nil {
				t.Fatalf("hold: %v", err)
			}
			tokens[i] = token
		}
		for i, token := range tokens {
			value, release, err := h.Resolve("g", token)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			want := round*MaxHandles + i
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer release()
				if value != want {
					t.Errorf("read value = %d, want %d", value, want)
				}
				if err := h.Release("g", token); err != nil {
					t.Errorf("release: %v", err)
				}
			}()
		}
		wg.Wait()
	}
	if h.LiveCount() != 0 || h.SharedCount() != 0 {
		t.Fatalf("leaked handles=%d shared=%d", h.LiveCount(), h.SharedCount())
	}
}
