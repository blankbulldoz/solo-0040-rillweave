package walk

import (
	"errors"
	"testing"

	"rillweave/graph"
	"rillweave/index"
)

func testGraph(t *testing.T) (*graph.Graph, *index.Adjacency) {
	t.Helper()
	schema := graph.Schema{
		Vertices: map[string]graph.VertexRule{
			"strut": {Properties: map[string]graph.PropertyRule{}},
		},
		Arcs: map[string]graph.ArcRule{
			"join":  {From: "strut", To: "strut"},
			"guide": {From: "strut", To: "strut"},
		},
	}
	g := graph.New("fixture", schema)
	for _, id := range []string{"hinge", "keel", "spar", "tip", "extra"} {
		g.Vertices[id] = graph.Vertex{ID: id, Kind: "strut"}
	}
	arcs := []graph.Arc{
		{ID: "brace", Kind: "join", From: "hinge", To: "spar", Cost: 8},
		{ID: "brace-alt", Kind: "join", From: "hinge", To: "spar", Cost: 8},
		{ID: "bypass", Kind: "join", From: "hinge", To: "keel", Cost: 1},
		{ID: "cross", Kind: "join", From: "keel", To: "spar", Cost: 1},
		{ID: "stem", Kind: "join", From: "spar", To: "tip", Cost: 1},
		{ID: "return", Kind: "join", From: "spar", To: "hinge", Cost: 2},
		{ID: "loop", Kind: "join", From: "hinge", To: "hinge", Cost: 1},
		{ID: "track", Kind: "guide", From: "tip", To: "hinge", Cost: 1},
	}
	for _, arc := range arcs {
		g.Arcs[arc.ID] = arc
	}
	return g, index.BuildAdjacency(g)
}

func baseQuery() PathRequest {
	return PathRequest{From: "hinge", To: "tip", MaxHops: 2, Selection: Selection{Kinds: []string{"join"}}}
}

// TestPathOneShot preserves the original budget exhaustion contract outside a
// session: no partial-looking result is ever returned.
func TestPathOneShot(t *testing.T) {
	g, adj := testGraph(t)
	result, err := Path(g, adj, baseQuery())
	if err != nil {
		t.Fatalf("one-shot path: %v", err)
	}
	if !result.Complete || !result.Found || result.Cost != 9 {
		t.Fatalf("unexpected result %+v", result)
	}
	if len(result.Arcs) != 2 || result.Arcs[0] != "brace" || result.Arcs[1] != "stem" {
		t.Fatalf("wrong witness %v", result.Arcs)
	}
	if _, err := Path(g, adj, PathRequest{
		From: "hinge", To: "tip", MaxHops: 2,
		Selection: Selection{Kinds: []string{"join"}, Budget: 1},
	}); err == nil {
		t.Fatal("budget exhaustion must reject the one-shot query")
	}
}

// TestPathResumeRework shows that every candidate arc is charged exactly once
// across arbitrarily small segments, and the segmented answer equals the
// one-shot answer.
func TestPathResumeNoRework(t *testing.T) {
	g, adj := testGraph(t)
	oneShot, err := Path(g, adj, PathRequest{From: "hinge", To: "tip", MaxHops: 3, Selection: Selection{Kinds: []string{"join"}}})
	if err != nil {
		t.Fatalf("one-shot: %v", err)
	}
	search, err := NewPathSearch(g, adj, PathRequest{From: "hinge", To: "tip", MaxHops: 3, Selection: Selection{Kinds: []string{"join"}}})
	if err != nil {
		t.Fatalf("new search: %v", err)
	}
	var result PathResult
	for segments := 0; !result.Complete; segments++ {
		if segments > oneShot.Scanned+1 {
			t.Fatal("search did not converge")
		}
		result, err = search.Advance(1)
		if err != nil {
			t.Fatalf("advance: %v", err)
		}
	}
	if result.Scanned != oneShot.Scanned {
		t.Fatalf("segmented scans %d differ from one-shot %d", result.Scanned, oneShot.Scanned)
	}
	if result.Cost != oneShot.Cost || !equalStrings(result.Arcs, oneShot.Arcs) {
		t.Fatalf("segmented witness %v differs from one-shot %v", result.Arcs, oneShot.Arcs)
	}
}

// TestPathUpperBoundPreliminary ensures a paused segment may report an upper
// bound without claiming completion, and the cheaper route is found later.
func TestPathUpperBoundPreliminary(t *testing.T) {
	g, adj := testGraph(t)
	q := PathRequest{From: "hinge", To: "tip", MaxHops: 3, Selection: Selection{Kinds: []string{"join"}}}
	search, err := NewPathSearch(g, adj, q)
	if err != nil {
		t.Fatalf("new search: %v", err)
	}
	first, err := search.Advance(11)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if first.Complete || !first.Found || first.UpperBound == nil || *first.UpperBound != 9 {
		t.Fatalf("expected provisional bound 9, got %+v", first)
	}
	second, err := search.Advance(7)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !second.Complete || second.Cost != 3 || second.UpperBound == nil || *second.UpperBound != 3 {
		t.Fatalf("expected final cost 3, got %+v", second)
	}
}

// TestPathAgreement guards the continuation contract: direction, arc kinds,
// endpoints and hop limit must repeat the original request.
func TestPathAgreement(t *testing.T) {
	g, adj := testGraph(t)
	q := baseQuery()
	search, err := NewPathSearch(g, adj, q)
	if err != nil {
		t.Fatalf("new search: %v", err)
	}
	identical := []PathRequest{
		q,
		{From: "hinge", To: "tip", MaxHops: 2, Selection: Selection{Kinds: []string{"join"}, Direction: "out"}},
		{From: "hinge", To: "tip", MaxHops: 2, Selection: Selection{Kinds: []string{"join", "join"}}},
	}
	for i, candidate := range identical {
		if !search.Agreement(candidate) {
			t.Fatalf("case %d must agree: %+v", i, candidate)
		}
	}
	changed := []PathRequest{
		{From: "keel", To: "tip", MaxHops: 2, Selection: Selection{Kinds: []string{"join"}}},
		{From: "hinge", To: "keel", MaxHops: 2, Selection: Selection{Kinds: []string{"join"}}},
		{From: "hinge", To: "tip", MaxHops: 3, Selection: Selection{Kinds: []string{"join"}}},
		{From: "hinge", To: "tip", MaxHops: 2, Selection: Selection{Kinds: []string{"guide"}}},
		{From: "hinge", To: "tip", MaxHops: 2, Selection: Selection{Kinds: []string{"join"}, Direction: "in"}},
	}
	for i, candidate := range changed {
		if search.Agreement(candidate) {
			t.Fatalf("case %d must disagree: %+v", i, candidate)
		}
	}
}

// TestPathNoRoute is a terminal, complete answer rather than exhaustion.
func TestPathNoRoute(t *testing.T) {
	g, adj := testGraph(t)
	q := PathRequest{From: "hinge", To: "tip", MaxHops: 1, Selection: Selection{Kinds: []string{"join"}}}
	result, err := Path(g, adj, q)
	if err != nil {
		t.Fatalf("no-route query: %v", err)
	}
	if !result.Complete || result.Found || result.UpperBound != nil {
		t.Fatalf("unexpected no-route result %+v", result)
	}
}

// TestPathZeroHops covers the trivial empty path and its hard validation.
func TestPathZeroHops(t *testing.T) {
	g, adj := testGraph(t)
	result, err := Path(g, adj, PathRequest{From: "hinge", To: "hinge", MaxHops: 0})
	if err != nil {
		t.Fatalf("empty path: %v", err)
	}
	if !result.Complete || !result.Found || len(result.Arcs) != 0 {
		t.Fatalf("unexpected empty-path result %+v", result)
	}
	if _, err := NewPathSearch(g, adj, PathRequest{From: "hinge", To: "tip", MaxHops: -1}); err == nil {
		t.Fatal("negative max_hops must be invalid")
	}
	if _, err := NewPathSearch(g, adj, PathRequest{From: "hinge", To: "ghost", MaxHops: 1}); err == nil {
		t.Fatal("missing endpoint must be invalid")
	}
}

func TestBudgetLimit(t *testing.T) {
	if limit, err := BudgetLimit(0); err != nil || limit != MaxBudget {
		t.Fatalf("default limit = %d, %v", limit, err)
	}
	if _, err := BudgetLimit(MaxBudget + 1); err == nil {
		t.Fatal("budget above the cap must be invalid")
	}
	if _, err := BudgetLimit(-1); err == nil {
		t.Fatal("negative budget must be invalid")
	}
}

// TestPathLabelCap exercises the real cross-frontier label bound. The fixture
// fans one root out over many distinct vertices, so a small temporary cap is
// reached as the next frontier grows alongside the surviving root label.
func TestPathLabelCap(t *testing.T) {
	g, adj := testGraph(t)
	original := MaxPathLabels
	t.Cleanup(func() { MaxPathLabels = original })
	MaxPathLabels = 2
	search, err := NewPathSearch(g, adj, PathRequest{From: "hinge", To: "tip", MaxHops: 3})
	if err != nil {
		t.Fatalf("new search: %v", err)
	}
	if _, err := search.Advance(MaxBudget); err == nil {
		t.Fatal("a search beyond the label cap must fail with a budget problem")
	} else {
		var problem *graph.Problem
		if !errors.As(err, &problem) || problem.Code != graph.Budget {
			t.Fatalf("expected budget_exhausted, got %v", err)
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
