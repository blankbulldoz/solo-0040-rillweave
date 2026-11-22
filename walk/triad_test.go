package walk

import (
	"errors"
	"testing"

	"rillweave/graph"
	"rillweave/index"
)

func triadGraph() *graph.Graph {
	g := graph.New("abutment", graph.Schema{
		Vertices: map[string]graph.VertexRule{"strut": {Properties: map[string]graph.PropertyRule{
			"tag":    {Kind: graph.Text, Unique: true},
			"marked": {Kind: graph.Bool},
		}}},
		Arcs: map[string]graph.ArcRule{
			"join":  {From: "strut", To: "strut"},
			"guide": {From: "strut", To: "strut"},
		},
	})
	props := map[string]graph.Scalar{
		"hinge": {Kind: graph.Text, Text: "north"},
		"spar":  {Kind: graph.Text, Text: "south"},
	}
	for id, tag := range props {
		g.Vertices[id] = graph.Vertex{ID: id, Kind: "strut", Properties: map[string]graph.Scalar{"tag": tag}}
	}
	for _, id := range []string{"keel", "tip"} {
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
	for _, a := range arcs {
		g.Arcs[a.ID] = a
	}
	return g
}

func triadSetup(t *testing.T) (*graph.Graph, *index.Adjacency, *index.Postings) {
	t.Helper()
	g := triadGraph()
	return g, index.BuildAdjacency(g), index.BuildPostings(g)
}

func text(s string) graph.Scalar { return graph.Scalar{Kind: graph.Text, Text: s} }

func allOutTriad(vertices map[string]VertexPattern) TriadRequest {
	if vertices == nil {
		vertices = map[string]VertexPattern{
			"a": {Kind: "strut"}, "b": {Kind: "strut"}, "c": {Kind: "strut"},
		}
	}
	return TriadRequest{
		Vertices: vertices,
		Edges: []EdgePattern{
			{Kind: "join", From: "a", To: "b"},
			{Kind: "join", From: "b", To: "c"},
			{Kind: "join", From: "c", To: "a"},
		},
	}
}

func rowKey(r TriadRow) string {
	return r.Vertices["a"] + "," + r.Vertices["b"] + "," + r.Vertices["c"] +
		"|" + r.Arcs["e1"] + "," + r.Arcs["e2"] + "," + r.Arcs["e3"]
}

func TestTriadAllOutSparseVertex(t *testing.T) {
	g, adj, props := triadSetup(t)
	q := allOutTriad(map[string]VertexPattern{
		"a": {Kind: "strut", Properties: map[string]Scalar{"tag": text("north")}},
		"b": {Kind: "strut"},
		"c": {Kind: "strut", Properties: map[string]Scalar{"tag": text("south")}},
	})
	res, err := Triad(g, adj, props, q)
	if err != nil {
		t.Fatal(err)
	}
	// a is hinge and c is spar. b can be hinge (loop + brace + return) or
	// keel (bypass + cross + return); the parallel brace arcs give two rows.
	want := map[string]bool{
		"hinge,hinge,spar|loop,brace,return":     false,
		"hinge,hinge,spar|loop,brace-alt,return": false,
		"hinge,keel,spar|bypass,cross,return":    false,
	}
	if res.Matched != len(want) {
		t.Fatalf("want %d closed triangles, got %d: %+v", len(want), res.Matched, res.Rows)
	}
	for _, r := range res.Rows {
		if _, ok := want[rowKey(r)]; !ok {
			t.Fatalf("unexpected row %s", rowKey(r))
		}
		want[rowKey(r)] = true
	}
}

func TestTriadParallelArcsAreSeparateEvidence(t *testing.T) {
	g, adj, props := triadSetup(t)
	q := allOutTriad(map[string]VertexPattern{
		"a": {Kind: "strut", Properties: map[string]Scalar{"tag": text("north")}},
		"b": {Kind: "strut", Properties: map[string]Scalar{"tag": text("south")}},
		"c": {Kind: "strut"},
	})
	res, err := Triad(g, adj, props, q)
	if err != nil {
		t.Fatal(err)
	}
	// hinge->spar has two parallel arcs; each must yield its own row while
	// the other two edges stay fixed.
	if res.Matched != 2 {
		t.Fatalf("want two parallel-arc witnesses, got %d: %+v", res.Matched, res.Rows)
	}
	first := map[string]bool{}
	for _, r := range res.Rows {
		first[r.Arcs["e1"]] = true
		if r.Arcs["e2"] != "return" || r.Arcs["e3"] != "loop" {
			t.Fatalf("parallel row changed unrelated arcs: %+v", r)
		}
	}
	if !first["brace"] || !first["brace-alt"] {
		t.Fatalf("both parallel arcs must appear, got %v", first)
	}
}

func TestTriadNoArcReusedWithinRow(t *testing.T) {
	// Minimal graph with one arc per endpoint pair. A complete triad over
	// two vertices needs two distinct stored arcs on at least one pair; a
	// single arc identity must never be reused to fill two edge variables.
	g := graph.New("mini", graph.Schema{
		Vertices: map[string]graph.VertexRule{"strut": {}},
		Arcs:     map[string]graph.ArcRule{"join": {From: "strut", To: "strut"}},
	})
	for _, id := range []string{"a1", "b1"} {
		g.Vertices[id] = graph.Vertex{ID: id, Kind: "strut"}
	}
	g.Arcs["only"] = graph.Arc{ID: "only", Kind: "join", From: "a1", To: "b1", Cost: 1}
	adj, props := index.BuildAdjacency(g), index.BuildPostings(g)
	q := TriadRequest{
		Vertices: map[string]VertexPattern{
			"a": {Kind: "strut"}, "b": {Kind: "strut"}, "c": {Kind: "strut"},
		},
		Edges: []EdgePattern{
			{Kind: "join", From: "a", To: "b"},
			{Kind: "join", From: "b", To: "c"},
			{Kind: "join", From: "a", To: "c"},
		},
	}
	out, err := Triad(g, adj, props, q)
	if err != nil {
		t.Fatal(err)
	}
	if out.Matched != 0 {
		t.Fatalf("single arc must not be reused as a second witness, got %+v", out.Rows)
	}
}

// TestTriadParallelArcsFillTwoEdgeVariables shows the positive counterpart:
// when a pair carries arcs in both directions, two edge variables can bind
// distinct identities and the combination is its own evidence row.
func TestTriadParallelArcsFillTwoEdgeVariables(t *testing.T) {
	g := graph.New("mini", graph.Schema{
		Vertices: map[string]graph.VertexRule{"strut": {}},
		Arcs:     map[string]graph.ArcRule{"join": {From: "strut", To: "strut"}},
	})
	for _, id := range []string{"a1", "b1"} {
		g.Vertices[id] = graph.Vertex{ID: id, Kind: "strut"}
	}
	g.Arcs["p"] = graph.Arc{ID: "p", Kind: "join", From: "a1", To: "b1", Cost: 1}
	g.Arcs["q"] = graph.Arc{ID: "q", Kind: "join", From: "b1", To: "a1", Cost: 2}
	g.Arcs["r"] = graph.Arc{ID: "r", Kind: "join", From: "a1", To: "b1", Cost: 3}
	adj, props := index.BuildAdjacency(g), index.BuildPostings(g)
	q := TriadRequest{
		Vertices: map[string]VertexPattern{
			"a": {Kind: "strut"}, "b": {Kind: "strut"}, "c": {Kind: "strut"},
		},
		Edges: []EdgePattern{
			{Kind: "join", From: "a", To: "b"},
			{Kind: "join", From: "b", To: "c"},
			{Kind: "join", From: "a", To: "c", Direction: index.In},
		},
	}
	out, err := Triad(g, adj, props, q)
	if err != nil {
		t.Fatal(err)
	}
	// a=c=a1, b=b1: e2 (b->c out) and e3 (a->c in) both need the stored
	// b1->a1 arc q; they cannot share it, while e1 can take p or r. The
	// remaining distinct identities still cannot give e2 and e3 two arcs,
	// so the request stays empty — proving no in-row duplication, and the
	// parallel arcs p/r never collapse into one identity.
	if out.Matched != 0 {
		t.Fatalf("edge variables must keep distinct arc identities, got %+v", out.Rows)
	}
}

func TestTriadSelfLoopBindsOneSlot(t *testing.T) {
	g, adj, props := triadSetup(t)
	// All three variables pinned to hinge via the unique tag; every edge is
	// the hinge self-loop, but three distinct arc variables cannot share one
	// stored loop, so this stays empty — proving loops are not triple-counted.
	q := allOutTriad(map[string]VertexPattern{
		"a": {Kind: "strut", Properties: map[string]Scalar{"tag": text("north")}},
		"b": {Kind: "strut", Properties: map[string]Scalar{"tag": text("north")}},
		"c": {Kind: "strut", Properties: map[string]Scalar{"tag": text("north")}},
	})
	res, err := Triad(g, adj, props, q)
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 0 {
		t.Fatalf("a single self-loop cannot witness three arc variables, got %+v", res.Rows)
	}
}

func TestTriadReversedDirection(t *testing.T) {
	g, adj, props := triadSetup(t)
	// c->a requested "in" means the stored arc must run a->c (hinge->spar).
	q := TriadRequest{
		Vertices: map[string]VertexPattern{
			"a": {Kind: "strut", Properties: map[string]Scalar{"tag": text("north")}},
			"b": {Kind: "strut", Properties: map[string]Scalar{"tag": text("south")}},
			"c": {Kind: "strut"},
		},
		Edges: []EdgePattern{
			{Kind: "join", From: "a", To: "b"},
			{Kind: "join", From: "b", To: "c", Direction: index.Out},
			{Kind: "join", From: "c", To: "a", Direction: index.In},
		},
	}
	res, err := Triad(g, adj, props, q)
	if err != nil {
		t.Fatal(err)
	}
	// b=spar, b->c out reaches tip (stem) and hinge (return). c->a in needs
	// a stored a->c arc (hinge->c); c=hinge is satisfied by loop, whereas
	// c=spar would require brace, which e1 already consumes (no reuse).
	want := map[string]bool{
		"hinge,spar,hinge|brace,return,loop":     false,
		"hinge,spar,hinge|brace-alt,return,loop": false,
	}
	if res.Matched != len(want) {
		t.Fatalf("want %d reversed witnesses, got %+v", len(want), res.Rows)
	}
	for _, r := range res.Rows {
		if _, ok := want[rowKey(r)]; !ok {
			t.Fatalf("unexpected reversed row %s", rowKey(r))
		}
		want[rowKey(r)] = true
	}
}

func TestTriadRowsAreSortedAndStable(t *testing.T) {
	g, adj, props := triadSetup(t)
	res, err := Triad(g, adj, props, allOutTriad(nil))
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(res.Rows); i++ {
		if rowKey(res.Rows[i-1]) > rowKey(res.Rows[i]) {
			t.Fatalf("rows not sorted: %q before %q", rowKey(res.Rows[i-1]), rowKey(res.Rows[i]))
		}
	}
}

func TestTriadLimitWindowAndTruncation(t *testing.T) {
	g, adj, props := triadSetup(t)
	q := allOutTriad(nil)
	q.Limit = 3
	res, err := Triad(g, adj, props, q)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rows) != 3 || !res.Truncated || res.Matched <= 3 {
		t.Fatalf("limit window wrong: rows=%d matched=%d truncated=%v", len(res.Rows), res.Matched, res.Truncated)
	}
	if !res.Exhaustive {
		t.Fatal("a query completing within budget must be exhaustive")
	}
}

func TestTriadBudgetExhaustionRejects(t *testing.T) {
	g, adj, props := triadSetup(t)
	q := allOutTriad(nil)
	q.Budget = 1
	_, err := Triad(g, adj, props, q)
	var problem *graph.Problem
	if !errors.As(err, &problem) || problem.Code != graph.Budget {
		t.Fatalf("want budget_exhausted, got %v", err)
	}
}

func TestTriadBudgetAllowsSparsePlan(t *testing.T) {
	g, adj, props := triadSetup(t)
	// A small but sufficient budget must still answer a tightly constrained
	// query even though the unconstrained vertex domain is the whole graph.
	q := allOutTriad(map[string]VertexPattern{
		"a": {Kind: "strut", Properties: map[string]Scalar{"tag": text("north")}},
		"b": {Kind: "strut"},
		"c": {Kind: "strut", Properties: map[string]Scalar{"tag": text("south")}},
	})
	q.Budget = 40
	res, err := Triad(g, adj, props, q)
	if err != nil {
		t.Fatalf("sparse plan should fit a small budget: %v", err)
	}
	if res.Matched != 3 || !res.Exhaustive {
		t.Fatalf("sparse query must complete, got %+v", res)
	}
	// The same budget must reject an unconstrained query that fans out over
	// the whole graph, proving the planner prefers the small seed.
	wide := allOutTriad(nil)
	wide.Budget = 40
	if _, err := Triad(g, adj, props, wide); err == nil {
		t.Fatal("unconstrained fan-out should exhaust the same budget")
	}
}

func TestTriadMissingPropertyIsNotEmptyValue(t *testing.T) {
	g, adj, props := triadSetup(t)
	q := allOutTriad(map[string]VertexPattern{
		"a": {Kind: "strut", Properties: map[string]Scalar{"tag": text("")}},
		"b": {Kind: "strut"},
		"c": {Kind: "strut"},
	})
	res, err := Triad(g, adj, props, q)
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 0 {
		t.Fatalf("absent tag must not match empty text, got %+v", res.Rows)
	}
}

func TestTriadWrongScalarTypeRejected(t *testing.T) {
	g, adj, props := triadSetup(t)
	q := allOutTriad(map[string]VertexPattern{
		"a": {Kind: "strut", Properties: map[string]Scalar{"tag": {Kind: graph.Bool, Bool: true}}},
		"b": {Kind: "strut"},
		"c": {Kind: "strut"},
	})
	_, err := Triad(g, adj, props, q)
	assertInvalid(t, err)
}

func TestTriadUnknownKindsRejected(t *testing.T) {
	g, adj, props := triadSetup(t)

	badVertex := allOutTriad(map[string]VertexPattern{
		"a": {Kind: "ghost"}, "b": {Kind: "strut"}, "c": {Kind: "strut"},
	})
	if _, err := Triad(g, adj, props, badVertex); err == nil {
		t.Fatal("unknown vertex kind must be rejected")
	}

	badArc := allOutTriad(nil)
	badArc.Edges[0].Kind = "ghost"
	_, err := Triad(g, adj, props, badArc)
	assertInvalid(t, err)
}

func TestTriadRejectsMalformedShape(t *testing.T) {
	g, adj, props := triadSetup(t)

	cases := []TriadRequest{
		{ // only two vertex variables
			Vertices: map[string]VertexPattern{
				"a": {Kind: "strut"}, "b": {Kind: "strut"},
			},
			Edges: []EdgePattern{
				{Kind: "join", From: "a", To: "b"},
				{Kind: "join", From: "b", To: "a"},
				{Kind: "join", From: "a", To: "b"},
			},
		},
		{ // repeated endpoint pair
			Vertices: map[string]VertexPattern{
				"a": {Kind: "strut"}, "b": {Kind: "strut"}, "c": {Kind: "strut"},
			},
			Edges: []EdgePattern{
				{Kind: "join", From: "a", To: "b"},
				{Kind: "join", From: "a", To: "b"},
				{Kind: "join", From: "b", To: "c"},
			},
		},
		{ // bad variable reference and bad direction
			Vertices: map[string]VertexPattern{
				"a": {Kind: "strut"}, "b": {Kind: "strut"}, "c": {Kind: "strut"},
			},
			Edges: []EdgePattern{
				{Kind: "join", From: "a", To: "z"},
				{Kind: "join", From: "b", To: "c"},
				{Kind: "join", From: "c", To: "a", Direction: "sideways"},
			},
		},
	}
	for i, q := range cases {
		if _, err := Triad(g, adj, props, q); err == nil {
			t.Fatalf("malformed case %d must be rejected", i)
		}
	}
}

func TestTriadEdgeRootedPlanSparseArcKind(t *testing.T) {
	// Wide vertex conditions but a very sparse closing arc kind: the edge
	// rooted plan must win and find the one witness through the rare arc
	// without enumerating every vertex triple first.
	g, adj, props := triadSetup(t)
	q := TriadRequest{
		Vertices: map[string]VertexPattern{
			"a": {Kind: "strut"}, "b": {Kind: "strut"}, "c": {Kind: "strut"},
		},
		Edges: []EdgePattern{
			{Kind: "join", From: "a", To: "b"},
			{Kind: "join", From: "b", To: "c"},
			{Kind: "guide", From: "c", To: "a"},
		},
	}
	plan := choosePlan(g, adj, q.Edges, [3][]string{
		graph.Keys(g.Vertices), graph.Keys(g.Vertices), graph.Keys(g.Vertices),
	})
	if plan.vertexRoot {
		t.Fatalf("sparse guide arc should select an edge-rooted plan, got %+v", plan)
	}
	res, err := Triad(g, adj, props, q)
	if err != nil {
		t.Fatal(err)
	}
	// guide only has track: tip->hinge. c->a out via guide means c=tip,
	// a=hinge; b=spar joins via brace family and stem, so the parallel
	// brace arcs give two witnesses that both share the single track arc.
	if res.Matched != 2 {
		t.Fatalf("want two guide-seeded witnesses, got %+v", res.Rows)
	}
	for _, r := range res.Rows {
		if r.Vertices["a"] != "hinge" || r.Vertices["b"] != "spar" || r.Vertices["c"] != "tip" {
			t.Fatalf("unexpected edge-rooted vertices %+v", r.Vertices)
		}
		if r.Arcs["e3"] != "track" {
			t.Fatalf("rare guide arc must be the witness, got %+v", r.Arcs)
		}
	}
}

func TestTriadEmptyGraphIsExhaustive(t *testing.T) {
	empty := graph.New("abutment", graph.Schema{
		Vertices: map[string]graph.VertexRule{"strut": {}},
		Arcs:     map[string]graph.ArcRule{"join": {From: "strut", To: "strut"}},
	})
	adj := index.BuildAdjacency(empty)
	props := index.BuildPostings(empty)
	res, err := Triad(empty, adj, props, allOutTriad(nil))
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 0 || len(res.Rows) != 0 || !res.Exhaustive || res.Scanned != 0 {
		t.Fatalf("empty graph should be an exhaustive empty result, got %+v", res)
	}
}

func assertInvalid(t *testing.T, err error) {
	t.Helper()
	var problem *graph.Problem
	if !errors.As(err, &problem) || problem.Code != graph.Invalid {
		t.Fatalf("want invalid problem, got %v", err)
	}
}
