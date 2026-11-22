package engine

import (
	"testing"

	"rillweave/change"
	"rillweave/graph"
	"rillweave/walk"
)

func schema() graph.Schema {
	return graph.Schema{
		Vertices: map[string]graph.VertexRule{
			"strut": {Properties: map[string]graph.PropertyRule{}},
		},
		Arcs: map[string]graph.ArcRule{
			"join": {From: "strut", To: "strut"},
		},
	}
}

func vertex(id string) change.Edit {
	return change.Edit{Op: "put_vertex", Vertex: &graph.Vertex{ID: id, Kind: "strut"}}
}

func arc(id, from, to string, cost int64) change.Edit {
	return change.Edit{Op: "put_arc", Arc: &graph.Arc{ID: id, Kind: "join", From: from, To: to, Cost: cost}}
}

func newTestDB(t *testing.T) *Database {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Create("abutment", schema()); err != nil {
		t.Fatalf("create: %v", err)
	}
	edits := []change.Edit{
		arc("brace", "hinge", "spar", 8),
		arc("bypass", "hinge", "keel", 1),
		arc("cross", "keel", "spar", 1),
		arc("stem", "spar", "tip", 1),
		vertex("hinge"), vertex("keel"), vertex("spar"), vertex("tip"),
	}
	if _, err := db.Commit("abutment", change.Batch{Base: 1, Edits: edits}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return db
}

func pathQuery(budget int) walk.PathRequest {
	return walk.PathRequest{
		From: "hinge", To: "tip", MaxHops: 2,
		Selection: walk.Selection{Kinds: []string{"join"}, Budget: budget},
	}
}

// TestEnginePathSessionLifecycle drives start, continuation, commit isolation
// and terminal auto-close through the public engine entry points.
func TestEnginePathSessionLifecycle(t *testing.T) {
	db := newTestDB(t)
	first, err := db.Path("abutment", "", pathQuery(1), "")
	if err != nil {
		t.Fatalf("start segment: %v", err)
	}
	if first.Result.Complete || first.Result.Session == "" || first.Result.Scanned != 1 {
		t.Fatalf("unexpected paused segment %+v", first.Result)
	}
	token := first.Result.Session

	// A later head must not disturb candidates pinned by the open session.
	if _, err := db.Commit("abutment", change.Batch{
		Base: 2, Edits: []change.Edit{vertex("wing")},
	}); err != nil {
		t.Fatalf("second commit: %v", err)
	}
	var resumed ReadResult[PathOutcome]
	for segments := 0; !resumed.Result.Complete; segments++ {
		if segments > 32 {
			t.Fatal("session did not converge")
		}
		resumed, err = db.Path("abutment", "", pathQuery(1), token)
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if resumed.Revision != 2 || resumed.Result.Session != token && !resumed.Result.Complete {
			t.Fatalf("session drifted off its original revision: %+v", resumed)
		}
	}
	if resumed.Revision != 2 || resumed.Result.Cost != 9 {
		t.Fatalf("session must finish on its original revision: %+v", resumed)
	}
	if resumed.Result.Session != "" {
		t.Fatal("terminal session must be closed and return no token")
	}
	if _, err := db.Path("abutment", "", pathQuery(1), token); err == nil {
		t.Fatal("a closed session token must not resolve")
	}
}

// TestEnginePathSessionAgreement rejects continuations that change the query.
func TestEnginePathSessionAgreement(t *testing.T) {
	db := newTestDB(t)
	first, err := db.Path("abutment", "", pathQuery(1), "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	token := first.Result.Session
	t.Cleanup(func() { _ = db.ReleasePath("abutment", token) })

	changed := pathQuery(1)
	changed.MaxHops = 3
	if _, err := db.Path("abutment", "", changed, token); err == nil {
		t.Fatal("changed hop limit must not reuse a session")
	}
	unknown, err := db.Path("abutment", "", pathQuery(1), "00000000000000000000000000000000")
	if err == nil || unknown.Result.Session != "" {
		t.Fatalf("unknown token must be rejected, got %+v %v", unknown, err)
	}
}

// TestEnginePathSessionViewConflict requires an explicit view to identify the
// same immutable state the session pinned.
func TestEnginePathSessionViewConflict(t *testing.T) {
	db := newTestDB(t)
	first, err := db.Path("abutment", "", pathQuery(1), "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	token := first.Result.Session
	t.Cleanup(func() { _ = db.ReleasePath("abutment", token) })

	if _, err := db.Commit("abutment", change.Batch{
		Base: 2, Edits: []change.Edit{vertex("wing")},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	pin, err := db.Hold("abutment")
	if err != nil {
		t.Fatalf("hold: %v", err)
	}
	if _, err := db.Path("abutment", pin.Handle, pathQuery(1), token); err == nil {
		t.Fatal("a view of the new head must not attach to a session on the old one")
	}
}

// TestEnginePathSessionCapacity enforces the process-wide session bound and
// shows explicit release frees a slot.
func TestEnginePathSessionCapacity(t *testing.T) {
	db := newTestDB(t)
	tokens := make([]string, MaxPathSessions)
	for i := range tokens {
		outcome, err := db.Path("abutment", "", pathQuery(1), "")
		if err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		tokens[i] = outcome.Result.Session
	}
	if _, err := db.Path("abutment", "", pathQuery(1), ""); err == nil {
		t.Fatal("the session registry must reject work beyond its cap")
	}
	if err := db.ReleasePath("abutment", tokens[0]); err != nil {
		t.Fatalf("release: %v", err)
	}
	outcome, err := db.Path("abutment", "", pathQuery(1), "")
	if err != nil || outcome.Result.Session == "" {
		t.Fatalf("a released slot must be reusable, got %+v %v", outcome, err)
	}
}

// TestEnginePathSessionGraphScope keeps tokens from working against another
// graph even when the query shape is identical.
func TestEnginePathSessionGraphScope(t *testing.T) {
	db := newTestDB(t)
	first, err := db.Path("abutment", "", pathQuery(1), "")
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	token := first.Result.Session
	t.Cleanup(func() { _ = db.ReleasePath("abutment", token) })
	if _, err := db.Create("pier", schema()); err != nil {
		t.Fatalf("create second graph: %v", err)
	}
	if _, err := db.Path("pier", "", pathQuery(1), token); err == nil {
		t.Fatal("a session token must not resolve on a foreign graph")
	}
}
