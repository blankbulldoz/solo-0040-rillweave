package compress

import (
	"testing"

	"rillweave/graph"
	"rillweave/index"
)

func testGraph(t *testing.T) *graph.Graph {
	t.Helper()
	schema := graph.Schema{
		Vertices: map[string]graph.VertexRule{"node": {}},
		Arcs: map[string]graph.ArcRule{
			"join":  {From: "node", To: "node"},
			"guide": {From: "node", To: "node"},
		},
	}
	g := graph.New("weave", schema)
	for _, id := range []string{"a1", "a2", "a3", "a4", "b1", "b2", "c1", "c2", "iso", "lone"} {
		g.Vertices[id] = graph.Vertex{ID: id, Kind: "node"}
	}
	arcs := []graph.Arc{
		// Cluster A: a1 -> a2 -> a3 -> a1 ring plus a costly detour via a4.
		{ID: "a-12", Kind: "join", From: "a1", To: "a2", Cost: 1},
		{ID: "a-23", Kind: "join", From: "a2", To: "a3", Cost: 1},
		{ID: "a-31", Kind: "join", From: "a3", To: "a1", Cost: 1},
		// Reaching a4 and returning to the ring is deliberately expensive, so a
		// boundary pair routed through a4 cannot beat the direct ring prefixes.
		{ID: "a-14", Kind: "join", From: "a1", To: "a4", Cost: 20},
		{ID: "a-41", Kind: "guide", From: "a4", To: "a1", Cost: 30},
		{ID: "a-24", Kind: "join", From: "a2", To: "a4", Cost: 20},
		// Cluster B ring.
		{ID: "b-12", Kind: "join", From: "b1", To: "b2", Cost: 1},
		{ID: "b-21", Kind: "join", From: "b2", To: "b1", Cost: 1},
		// Cluster C ring.
		{ID: "c-12", Kind: "join", From: "c1", To: "c2", Cost: 1},
		{ID: "c-21", Kind: "join", From: "c2", To: "c1", Cost: 1},
		// Parallel cross arcs A -> B: expensive direct, cheap guide detour.
		{ID: "x-ab-costly", Kind: "join", From: "a3", To: "b1", Cost: 40},
		{ID: "x-ab-cheap", Kind: "guide", From: "a1", To: "b1", Cost: 5},
		// B -> C cross arc, only attached at b2.
		{ID: "x-bc", Kind: "join", From: "b2", To: "c1", Cost: 2},
		// A -> C exists but leaves from a4: tempts a cheap prefix that cannot
		// connect back to the agreed exit without paying the guide loop.
		{ID: "x-ac-trap", Kind: "join", From: "a4", To: "c2", Cost: 1},
		// Unreachable sink: nothing ever points into lone.
		{ID: "x-iso-lone", Kind: "join", From: "iso", To: "lone", Cost: 1},
	}
	for _, arc := range arcs {
		g.Arcs[arc.ID] = arc
	}
	return g
}

func buildTestNetwork(t *testing.T, kinds []string, endpoints []string) (*Network, *Document) {
	t.Helper()
	g := testGraph(t)
	doc, err := Build(g, index.BuildAdjacency(g), Request{
		Name: "weave-mini", Kinds: kinds, Endpoints: endpoints, Budget: MaxBuildBudget,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	envelope, err := Encode(doc)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := Decode(envelope)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	network, err := Materialize(decoded)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	return network, decoded
}

func TestCompressDropsInternalVertices(t *testing.T) {
	network, doc := buildTestNetwork(t, []string{"join", "guide"},
		[]string{"a1", "a3", "b1", "b2", "c1", "c2", "lone"})
	// a2 is purely internal to cluster A: no cross arc, no agreed endpoint.
	if _, ok := network.clusterOf["a2"]; ok {
		t.Fatal("internal vertex a2 must not be retained as a boundary")
	}
	// iso keeps a boundary identity because it sources a cross-group arc.
	for _, kept := range []string{"a1", "a3", "a4", "b1", "b2", "c1", "c2", "iso", "lone"} {
		if _, ok := network.clusterOf[kept]; !ok {
			t.Fatalf("boundary vertex %q must be retained", kept)
		}
	}
	if doc.Stats.RetainedArcs >= doc.Stats.OriginalArcs {
		t.Fatalf("expected compression, retained=%d original=%d",
			doc.Stats.RetainedArcs, doc.Stats.OriginalArcs)
	}
	if len(doc.ArcDict) == 0 {
		t.Fatal("arc dictionary must keep witness and cross records")
	}
}

func TestExactRouteAndReplay(t *testing.T) {
	network, _ := buildTestNetwork(t, []string{"join", "guide"},
		[]string{"a1", "a3", "b1", "b2", "c1", "c2", "lone"})

	// a1 -> b1 cheapest is the cheap guide cross arc directly (cost 5).
	answer, err := network.Route(RouteRequest{From: "a1", To: "b1"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if !answer.Found || !answer.Exact || answer.Cost != 5 {
		t.Fatalf("a1->b1 exact route mismatch: %+v", answer)
	}
	if answer.Arcs[0] != "x-ab-cheap" {
		t.Fatalf("expected cheap parallel arc, got %v", answer.Arcs)
	}
	verify := network.Replay(answer.Arcs)
	if !verify.Valid || verify.Cost != answer.Cost ||
		len(verify.Vertices) != len(answer.Arcs)+1 {
		t.Fatalf("replay mismatch: %+v", verify)
	}

	// a3 -> c1: must pay into b1/b2 then the B->C arc. A summary that spliced
	// the costly a4 detour would invent a cheaper route; verify the true cost.
	answer, err = network.Route(RouteRequest{From: "a3", To: "c1"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if !answer.Found || !answer.Exact {
		t.Fatalf("a3->c1 must be an exact found route: %+v", answer)
	}
	// a3->a1 (1), a1->b1 guide (5), b1->b2 (1), b2->c1 (2) = 9.
	if answer.Cost != 9 {
		t.Fatalf("a3->c1 cost = %d, want 9; arcs=%v", answer.Cost, answer.Arcs)
	}
	verify = network.Replay(answer.Arcs)
	if !verify.Valid || verify.Cost != 9 || verify.Vertices[0] != "a3" ||
		verify.Vertices[len(verify.Vertices)-1] != "c1" {
		t.Fatalf("a3->c1 replay not a contiguous witness: %+v", verify)
	}
}

func TestCheapButDisconnectedTrap(t *testing.T) {
	network, _ := buildTestNetwork(t, []string{"join", "guide"},
		[]string{"a1", "a3", "b1", "b2", "c1", "c2", "lone"})

	// From a3 to c2 the honest walk is a3->a1->b1->b2->c1->c2. The trap
	// x-ac-trap leaves a4, but reaching a4 from a3 costs a-31 + a-14 = 21
	// before the cross arc: a cluster-level merge that only stores one minimum
	// per group would splice an unrealistically cheap route through a4.
	answer, err := network.Route(RouteRequest{From: "a3", To: "c2"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if !answer.Found || !answer.Exact {
		t.Fatalf("a3->c2 exact route expected: %+v", answer)
	}
	// a3->a1 1 + guide 5 + b1->b2 1 + x-bc 2 + c1->c2 1 = 10.
	if answer.Cost <= 9 {
		t.Fatalf("trap was spliced: cost=%d arcs=%v", answer.Cost, answer.Arcs)
	}
	if answer.Cost != 10 {
		t.Fatalf("a3->c2 cost = %d, want 10; arcs=%v", answer.Cost, answer.Arcs)
	}
	for _, id := range answer.Arcs {
		if id == "x-ac-trap" {
			t.Fatalf("witness used the disconnected cheap trap: %v", answer.Arcs)
		}
	}

	// The same trap remains a genuinely valid, honest route from a4 itself;
	// boundary-level storage keeps it reachable rather than deleting it.
	fromTrap, err := network.Route(RouteRequest{From: "a4", To: "c2"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if !fromTrap.Found || fromTrap.Cost != 1 || len(fromTrap.Arcs) != 1 ||
		fromTrap.Arcs[0] != "x-ac-trap" {
		t.Fatalf("a4->c2 must use the trap arc directly: %+v", fromTrap)
	}
}

func TestSharedWitnessSegments(t *testing.T) {
	network, doc := buildTestNetwork(t, []string{"join", "guide"},
		[]string{"a1", "a3", "b1", "b2", "c1", "c2", "lone"})
	shared := 0
	for _, edge := range doc.Summaries {
		if edge.Shared {
			shared++
		}
	}
	if shared == 0 {
		t.Fatal("ring prefixes reused by several boundary pairs must be marked shared")
	}
	// Routes sharing a middle segment must still replay independently.
	first, err := network.Route(RouteRequest{From: "a3", To: "b1"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	second, err := network.Route(RouteRequest{From: "a3", To: "c1"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if first.Arcs[0] != "a-31" || second.Arcs[0] != "a-31" {
		t.Fatalf("both routes should share the a-31 prefix: %v %v", first.Arcs, second.Arcs)
	}
	for _, answer := range []RouteAnswer{first, second} {
		if verify := network.Replay(answer.Arcs); !verify.Valid || verify.Cost != answer.Cost {
			t.Fatalf("shared-segment witness failed replay: %+v", verify)
		}
	}
}

func TestDefinitiveNoRoute(t *testing.T) {
	network, _ := buildTestNetwork(t, []string{"join", "guide"},
		[]string{"a1", "a3", "b1", "b2", "c1", "c2", "lone"})
	answer, err := network.Route(RouteRequest{From: "a1", To: "lone"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if answer.Found || !answer.Exact || answer.Tier != tierExact {
		t.Fatalf("a1->lone must be an exact definitive no-route: %+v", answer)
	}
}

func TestOverviewMarking(t *testing.T) {
	// Exact set omits b2 and never includes same-cluster pairs.
	network, _ := buildTestNetwork(t, []string{"join", "guide"},
		[]string{"a1", "b1", "c1", "lone"})

	// Same cluster, both endpoints: overview even though route exists.
	answer, err := network.Route(RouteRequest{From: "a1", To: "a3"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if answer.Exact || answer.Tier != tierOverview || !answer.Found {
		t.Fatalf("same-cluster pair must stay overview: %+v", answer)
	}

	// Hop bound downgrades an otherwise exact cross-cluster query.
	answer, err = network.Route(RouteRequest{From: "a1", To: "c1", MaxHops: 32})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if answer.Exact || answer.Tier != tierOverview {
		t.Fatalf("hop-bounded query must be overview: %+v", answer)
	}

	// Non-endpoint boundary vertex yields overview, not an error.
	answer, err = network.Route(RouteRequest{From: "b2", To: "c1"})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if answer.Exact || !answer.Found {
		t.Fatalf("non-endpoint boundary route is overview but still answered: %+v", answer)
	}

	// Vertices not retained at all are missing, not silently overview.
	if _, err := network.Route(RouteRequest{From: "a2", To: "c1"}); err == nil {
		t.Fatal("unretained vertex must return a missing-vertex error")
	}
}

func TestKindSelectionChangesClusters(t *testing.T) {
	g := testGraph(t)
	doc, err := Build(g, index.BuildAdjacency(g), Request{
		Name: "join-only", Kinds: []string{"join"},
		Endpoints: []string{"a1", "a4", "b1", "c1"}, Budget: MaxBuildBudget,
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// Without guide, a4 only has outgoing a-14/a-24 incoming and no return via
	// guide a-41, so it forms its own cluster; x-ab-cheap (guide) disappears.
	for _, arc := range doc.CrossArcs {
		if arc.ArcID == "x-ab-cheap" {
			t.Fatal("unselected guide cross arc must not be retained")
		}
	}
	foundTrap := false
	for _, arc := range doc.CrossArcs {
		if arc.ArcID == "x-ac-trap" {
			foundTrap = true
		}
	}
	if !foundTrap {
		t.Fatal("a4 should be a separate cluster reachable via the join trap arc")
	}
}

func TestWitnessReplayDetectsTampering(t *testing.T) {
	network, _ := buildTestNetwork(t, []string{"join", "guide"},
		[]string{"a1", "b1"})
	bad := network.Replay([]string{"a-12", "b-12"})
	if bad.Valid || bad.FirstProblem == "" {
		t.Fatalf("non-contiguous witness must fail replay: %+v", bad)
	}
	missing := network.Replay([]string{"ghost-arc"})
	if missing.Valid || missing.FirstProblem == "" {
		t.Fatalf("unknown arc must fail replay: %+v", missing)
	}
}

func TestBuildRejectsBadRequests(t *testing.T) {
	g := testGraph(t)
	adj := index.BuildAdjacency(g)
	cases := []Request{
		{Name: "bad name", Endpoints: []string{"a1"}},
		{Name: "ok", Endpoints: nil},
		{Name: "ok", Endpoints: []string{"ghost"}},
		{Name: "ok", Endpoints: []string{"a1", "a1"}},
		{Name: "ok", Endpoints: []string{"a1"}, Kinds: []string{"unknown"}},
		{Name: "ok", Endpoints: []string{"a1"}, Budget: MaxBuildBudget + 1},
	}
	for i, req := range cases {
		if _, err := Build(g, adj, req); err == nil {
			t.Fatalf("case %d expected rejection", i)
		}
	}
}
