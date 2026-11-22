package engine

import (
	"sync"
	"testing"

	"rillweave/catalog"
	"rillweave/change"
	"rillweave/graph"
	"rillweave/walk"
)

func sessionGraph() *graph.Graph {
	g := graph.New("abutment", graph.Schema{
		Vertices: map[string]graph.VertexRule{"strut": {}},
		Arcs:     map[string]graph.ArcRule{"join": {From: "strut", To: "strut"}},
	})
	for _, id := range []string{"r", "a", "b", "c"} {
		g.Vertices[id] = graph.Vertex{ID: id, Kind: "strut"}
	}
	for _, arc := range []graph.Arc{
		{ID: "x1", Kind: "join", From: "r", To: "a", Cost: 1},
		{ID: "x2", Kind: "join", From: "a", To: "b", Cost: 1},
		{ID: "x3", Kind: "join", From: "b", To: "c", Cost: 1},
	} {
		g.Arcs[arc.ID] = arc
	}
	if err := catalog.Validate(g); err != nil {
		panic(err)
	}
	return g
}

func newTestDatabase(t *testing.T) *Database {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.publish(sessionGraph()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return db
}

func TestReachSessionStitchesFullTableAcrossWrites(t *testing.T) {
	db := newTestDatabase(t)
	opened, err := db.OpenReachSession("abutment", "", walk.ReachPageRequest{
		Root: "r", MaxHops: 3, Selection: walk.Selection{Budget: 1},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	handle := opened.Result.Handle
	if opened.Result.Page.Distances[0] != (walk.Distance{Vertex: "r", Hops: 0}) {
		t.Fatalf("opening page: %+v", opened.Result.Page)
	}
	var stitched []walk.Distance
	stitched = append(stitched, opened.Result.Page.Distances...)
	// Advance once, then move the head while the session is mid-traversal.
	first, err := db.AdvanceReachSession("abutment", handle, walk.ReachContinue{Budget: 1})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	stitched = append(stitched, first.Result.Distances...)
	summary, err := db.Commit("abutment", change.Batch{
		Base:  1,
		Edits: []change.Edit{{Op: "put_vertex", Vertex: &graph.Vertex{ID: "new", Kind: "strut"}}},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if summary.Revision != 2 {
		t.Fatalf("head revision = %d, want 2", summary.Revision)
	}
	for {
		page, err := db.AdvanceReachSession("abutment", handle, walk.ReachContinue{Budget: 1})
		if err != nil {
			t.Fatalf("advance: %v", err)
		}
		if page.Revision != 1 {
			t.Fatalf("session drifted to revision %d, want pinned 1", page.Revision)
		}
		stitched = append(stitched, page.Result.Distances...)
		if page.Result.Complete {
			break
		}
	}
	want := map[string]int{"r": 0, "a": 1, "b": 2, "c": 3}
	got := map[string]int{}
	for _, d := range stitched {
		got[d.Vertex] = d.Hops
	}
	if len(got) != len(want) {
		t.Fatalf("stitched = %v, want %v", got, want)
	}
	for v, h := range want {
		if got[v] != h {
			t.Fatalf("vertex %q hops %d, want %d", v, got[v], h)
		}
	}
	// The post-commit vertex is invisible to the pinned session but present at
	// the head, proving the snapshot pin rather than a stale cache.
	if _, err = db.Vertex("abutment", "", "new"); err != nil {
		t.Fatalf("head vertex: %v", err)
	}
	progress, err := db.ReachSessionProgress("abutment", handle)
	if err != nil {
		t.Fatalf("progress: %v", err)
	}
	if !progress.Result.Complete || progress.Result.Revision != 1 {
		t.Fatalf("progress = %+v", progress)
	}
}

func TestReachSessionCompletionIsDistinctFromLostHandle(t *testing.T) {
	db := newTestDatabase(t)
	opened, err := db.OpenReachSession("abutment", "", walk.ReachPageRequest{
		Root: "r", MaxHops: 1, Selection: walk.Selection{Budget: 1},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	handle := opened.Result.Handle
	for {
		page, err := db.AdvanceReachSession("abutment", handle, walk.ReachContinue{Budget: 1})
		if err != nil {
			t.Fatalf("advance: %v", err)
		}
		if page.Result.Complete {
			break
		}
	}
	again, err := db.AdvanceReachSession("abutment", handle, walk.ReachContinue{Budget: 1})
	if err != nil {
		t.Fatalf("advance finished session: %v", err)
	}
	if !again.Result.Complete || len(again.Result.Distances) != 0 {
		t.Fatalf("finished session page = %+v", again.Result)
	}
	if _, err = db.AdvanceReachSession("abutment", "missing", walk.ReachContinue{Budget: 1}); err == nil {
		t.Fatal("unknown handle was accepted")
	}
	if problem, ok := err.(*graph.Problem); !ok || problem.Code != graph.Missing {
		t.Fatalf("unknown handle error = %v, want not_found", err)
	}
	if err = db.CloseReachSession("abutment", handle); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err = db.ReachSessionProgress("abutment", handle); err == nil {
		t.Fatal("released handle still resolves")
	}
}

func TestReachSessionRejectsInvalidOpen(t *testing.T) {
	db := newTestDatabase(t)
	if _, err := db.OpenReachSession("abutment", "", walk.ReachPageRequest{
		Root: "ghost", MaxHops: 2,
	}); err == nil {
		t.Fatal("missing root was accepted")
	}
	if _, err := db.OpenReachSession("abutment", "", walk.ReachPageRequest{
		Root: "r", MaxHops: 99,
	}); err == nil {
		t.Fatal("hop bound above cap was accepted")
	}
	if len(db.reach.live) != 0 {
		t.Fatalf("failed opens leaked sessions: %d", len(db.reach.live))
	}
}

func TestReachSessionsConcurrentWithCommits(t *testing.T) {
	db := newTestDatabase(t)
	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			opened, err := db.OpenReachSession("abutment", "", walk.ReachPageRequest{
				Root: "r", MaxHops: 3, Selection: walk.Selection{Budget: 2},
			})
			if err != nil {
				t.Errorf("open: %v", err)
				return
			}
			handle := opened.Result.Handle
			defer db.CloseReachSession("abutment", handle)
			var rows []walk.Distance
			rows = append(rows, opened.Result.Page.Distances...)
			for {
				page, err := db.AdvanceReachSession("abutment", handle, walk.ReachContinue{Budget: 2})
				if err != nil {
					t.Errorf("advance: %v", err)
					return
				}
				rows = append(rows, page.Result.Distances...)
				if page.Result.Complete {
					break
				}
			}
			// The pinned revision 1 always yields the same minimum-hop table.
			got := map[string]int{}
			for _, d := range rows {
				got[d.Vertex] = d.Hops
			}
			want := map[string]int{"r": 0, "a": 1, "b": 2, "c": 3}
			if len(got) != len(want) {
				t.Errorf("pinned table = %v, want %v", got, want)
			}
		}()
	}
	// Concurrent commits move the head while sessions are mid-traversal.
	for i := 0; i < 5; i++ {
		v := graph.Vertex{ID: "extra" + string(rune('a'+i)), Kind: "strut"}
		if _, err := db.Commit("abutment", change.Batch{
			Base:  uint64(i + 1),
			Edits: []change.Edit{{Op: "put_vertex", Vertex: &v}},
		}); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}
	wg.Wait()
}
