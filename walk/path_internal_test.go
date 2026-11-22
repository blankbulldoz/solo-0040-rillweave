package walk

import (
	"errors"
	"slices"
	"testing"

	"rillweave/graph"
	"rillweave/index"
)

// fixture mirrors the walkthrough abutment graph. It contains a cheap prefix
// that is only reachable at a larger exact depth (bypass/cross/stem, cost 3
// over three arcs), an equal-cost parallel pair (brace / brace-alt, cost 8
// each), a reverse-walkable route (the stored brace arc read with direction
// in), and a guide arc (track) that kind selection rejects but billing keeps.
func fixture() (*graph.Graph, *index.Adjacency) {
	kinds := []string{"strut"}
	g := &graph.Graph{
		Name: "abutment", Revision: 2,
		Schema: graph.Schema{
			Vertices: map[string]graph.VertexRule{"strut": {}},
			Arcs: map[string]graph.ArcRule{
				"join":  {From: "strut", To: "strut"},
				"guide": {From: "strut", To: "strut"},
			},
		},
		Vertices: make(map[string]graph.Vertex),
		Arcs:     make(map[string]graph.Arc),
	}
	for _, id := range []string{"hinge", "keel", "spar", "tip"} {
		g.Vertices[id] = graph.Vertex{ID: id, Kind: kinds[0]}
	}
	arcs := []graph.Arc{
		{ID: "brace", Kind: "join", From: "hinge", To: "spar", Cost: 8},
		{ID: "brace-alt", Kind: "join", From: "hinge", To: "spar", Cost: 8},
		{ID: "bypass", Kind: "join", From: "hinge", To: "keel", Cost: 1},
		{ID: "cross", Kind: "join", From: "keel", To: "spar", Cost: 1},
		{ID: "loop", Kind: "join", From: "hinge", To: "hinge", Cost: 1},
		{ID: "return", Kind: "join", From: "spar", To: "hinge", Cost: 2},
		{ID: "stem", Kind: "join", From: "spar", To: "tip", Cost: 1},
		{ID: "track", Kind: "guide", From: "tip", To: "hinge", Cost: 1},
	}
	for _, arc := range arcs {
		g.Arcs[arc.ID] = arc
	}
	return g, index.BuildAdjacency(g)
}

// TestPathCandidateCompetition checks which label wins at each exact depth.
// Route choice is the only concern here; coin counts are asserted separately.
func TestPathCandidateCompetition(t *testing.T) {
	g, a := fixture()
	cases := []struct {
		name   string
		req    PathRequest
		found  bool
		cost   int64
		arcs   []string
		vertex []string
	}{
		{
			name:  "cheapest prefix inside two hops",
			req:   PathRequest{From: "hinge", To: "tip", MaxHops: 2, Selection: Selection{Kinds: []string{"join"}}},
			found: true, cost: 9, arcs: []string{"brace", "stem"},
			vertex: []string{"hinge", "spar", "tip"},
		},
		{
			name:  "cheaper prefix only reachable at three hops",
			req:   PathRequest{From: "hinge", To: "tip", MaxHops: 3, Selection: Selection{Kinds: []string{"join"}}},
			found: true, cost: 3, arcs: []string{"bypass", "cross", "stem"},
			vertex: []string{"hinge", "keel", "spar", "tip"},
		},
		{
			name:  "no route inside one hop",
			req:   PathRequest{From: "hinge", To: "tip", MaxHops: 1, Selection: Selection{Kinds: []string{"join"}}},
			found: false,
		},
		{
			name: "reverse-walkable stored arc",
			req: PathRequest{From: "tip", To: "hinge", MaxHops: 2, Selection: Selection{
				Direction: index.In, Kinds: []string{"join"},
			}},
			found: true, cost: 9, arcs: []string{"stem", "brace"},
			vertex: []string{"tip", "spar", "hinge"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Path(g, a, tc.req)
			if err != nil {
				t.Fatalf("Path: %v", err)
			}
			if result.Found != tc.found {
				t.Fatalf("found = %v, want %v", result.Found, tc.found)
			}
			if !tc.found {
				if len(result.Vertices) != 0 || len(result.Arcs) != 0 {
					t.Fatalf("no-route result carries a witness: %+v", result)
				}
				return
			}
			if result.Cost != tc.cost {
				t.Errorf("cost = %d, want %d", result.Cost, tc.cost)
			}
			if !slices.Equal(result.Arcs, tc.arcs) {
				t.Errorf("arcs = %v, want %v", result.Arcs, tc.arcs)
			}
			if !slices.Equal(result.Vertices, tc.vertex) {
				t.Errorf("vertices = %v, want %v", result.Vertices, tc.vertex)
			}
		})
	}
}

// TestParallelTieDeterministic pins the equal-cost parallel-arc tie on arc
// identity alone. brace and brace-alt both run hinge->spar at cost 8; only the
// smaller stored arc ID may witness the route, at every exact depth where the
// two compete.
func TestParallelTieDeterministic(t *testing.T) {
	g, a := fixture()
	tie := PathRequest{From: "hinge", To: "spar", MaxHops: 1, Selection: Selection{Kinds: []string{"join"}}}
	for _, budget := range []int{0, 10, MaxBudget} {
		req := tie
		req.Budget = budget
		result, err := Path(g, a, req)
		if err != nil {
			t.Fatalf("Path budget=%d: %v", budget, err)
		}
		if result.Cost != 8 || !slices.Equal(result.Arcs, []string{"brace"}) {
			t.Errorf("budget=%d witness = %v cost %d, want [brace] cost 8",
				budget, result.Arcs, result.Cost)
		}
	}

	// Reversing input insertion order must not move the tie.
	reordered := &graph.Graph{
		Name: "abutment", Revision: 2, Schema: g.Schema,
		Vertices: g.Vertices, Arcs: make(map[string]graph.Arc),
	}
	for _, id := range []string{"track", "stem", "return", "loop", "cross", "bypass", "brace-alt", "brace"} {
		reordered.Arcs[id] = g.Arcs[id]
	}
	shuffled, err := Path(reordered, index.BuildAdjacency(reordered), tie)
	if err != nil {
		t.Fatalf("Path reordered: %v", err)
	}
	if !slices.Equal(shuffled.Arcs, []string{"brace"}) {
		t.Errorf("reordered tie = %v, want [brace]", shuffled.Arcs)
	}
}

// TestWitnessOrderAndReconstruction pins the stored arc order independently of
// the representation. The chain is immutable, so two extensions of one prefix
// cannot alias each other, and materialization follows traversal order.
func TestWitnessOrderAndReconstruction(t *testing.T) {
	base := witnessAt("hinge")
	brace := base.extend(graph.Arc{ID: "brace", From: "hinge", To: "spar"}, "spar")
	if got := brace.arcs(); !slices.Equal(got, []string{"brace"}) {
		t.Fatalf("one-step arcs = %v", got)
	}
	tip := brace.extend(graph.Arc{ID: "stem", From: "spar", To: "tip"}, "tip")
	if got := tip.arcs(); !slices.Equal(got, []string{"brace", "stem"}) {
		t.Fatalf("two-step arcs = %v", got)
	}
	if got := tip.vertices(); !slices.Equal(got, []string{"hinge", "spar", "tip"}) {
		t.Fatalf("two-step vertices = %v", got)
	}
	// A sibling extension must not disturb the already materialized chain.
	keel := base.extend(graph.Arc{ID: "bypass", From: "hinge", To: "keel"}, "keel")
	if got := keel.arcs(); !slices.Equal(got, []string{"bypass"}) {
		t.Errorf("sibling arcs = %v, want [bypass]", got)
	}
	if got := brace.arcs(); !slices.Equal(got, []string{"brace"}) {
		t.Errorf("earlier chain changed to %v", got)
	}
	if base.steps != 0 || brace.steps != 1 || tip.steps != 2 {
		t.Errorf("steps = %d/%d/%d, want 0/1/2", base.steps, brace.steps, tip.steps)
	}

	// Reconstructed witnesses must agree with the immutable arc identities in
	// the graph, in traversal order, for both stored and reversed steps.
	g, a := fixture()
	for _, req := range []PathRequest{
		{From: "hinge", To: "tip", MaxHops: 3, Selection: Selection{Kinds: []string{"join"}}},
		{From: "tip", To: "hinge", MaxHops: 2, Selection: Selection{Direction: index.In, Kinds: []string{"join"}}},
	} {
		result, err := Path(g, a, req)
		if err != nil || !result.Found {
			t.Fatalf("Path(%v): %v, found=%v", req, err, result.Found)
		}
		if len(result.Arcs)+1 != len(result.Vertices) {
			t.Fatalf("witness shape: %d arcs, %d vertices", len(result.Arcs), len(result.Vertices))
		}
		for i, id := range result.Arcs {
			arc := g.Arcs[id]
			from, to := result.Vertices[i], result.Vertices[i+1]
			if req.Direction == index.In {
				from, to = to, from
			}
			if arc.From != from || arc.To != to {
				t.Errorf("step %d arc %q runs %s->%s, witness visits %s->%s",
					i, id, arc.From, arc.To, from, to)
			}
		}
	}
}

// TestScanBilling pins candidate accounting independently of route choice. The
// same fixture shows filtered candidates still billed, repeated inspections
// billed per visit, and the published boundary: exactly allowance inspections
// succeed and the next one fails the whole query.
func TestScanBilling(t *testing.T) {
	g, a := fixture()
	path := PathRequest{From: "hinge", To: "tip", MaxHops: 2, Selection: Selection{Kinds: []string{"join"}}}
	result, err := Path(g, a, path)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if result.Scanned != 11 {
		t.Fatalf("two-hop scanned = %d, want 11", result.Scanned)
	}
	if result.Cost != 9 || !slices.Equal(result.Arcs, []string{"brace", "stem"}) {
		t.Fatalf("route drifted while checking billing: %+v", result)
	}

	atLimit := path
	atLimit.Budget = result.Scanned
	if got, err := Path(g, a, atLimit); err != nil || got.Scanned != result.Scanned {
		t.Fatalf("exact allowance rejected: scanned=%d err=%v", got.Scanned, err)
	}
	justUnder := path
	justUnder.Budget = result.Scanned - 1
	if _, err := Path(g, a, justUnder); !isBudget(err) {
		t.Fatalf("allowance one short: err = %v, want budget exhaustion", err)
	}

	deeper := path
	deeper.MaxHops = 3
	long, err := Path(g, a, deeper)
	if err != nil {
		t.Fatalf("three-hop Path: %v", err)
	}
	if long.Scanned != 18 {
		t.Fatalf("three-hop scanned = %d, want 18", long.Scanned)
	}
	if long.Cost != 3 || !slices.Equal(long.Arcs, []string{"bypass", "cross", "stem"}) {
		t.Fatalf("three-hop route drifted while checking billing: %+v", long)
	}

	// track is a guide arc: selection rejects it, but inspection is billed.
	reach, err := Reach(g, a, ReachRequest{Root: "tip", MaxHops: 1, Selection: Selection{Kinds: []string{"join"}}})
	if err != nil {
		t.Fatalf("Reach: %v", err)
	}
	if reach.Scanned != 1 {
		t.Errorf("filtered arc scanned = %d, want 1", reach.Scanned)
	}
	if !slices.Equal(reach.Distances, []Distance{{Vertex: "tip", Hops: 0}}) {
		t.Errorf("filtered arc leaked into result: %+v", reach.Distances)
	}
}

func isBudget(err error) bool {
	var problem *graph.Problem
	return errors.As(err, &problem) && problem.Code == graph.Budget
}
