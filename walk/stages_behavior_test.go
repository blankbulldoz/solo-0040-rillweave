package walk

import (
	"testing"

	"rillweave/graph"
	"rillweave/index"
)

func buildGraph(t *testing.T, vertices []string, arcs []graph.Arc) (*graph.Graph, *index.Adjacency) {
	t.Helper()
	schema := graph.Schema{
		Vertices: map[string]graph.VertexRule{"strut": {}},
		Arcs:     map[string]graph.ArcRule{"join": {From: "strut", To: "strut"}},
	}
	g := graph.New("cap", schema)
	g.Vertices = map[string]graph.Vertex{}
	for _, v := range vertices {
		g.Vertices[v] = graph.Vertex{ID: v, Kind: "strut"}
	}
	g.Arcs = map[string]graph.Arc{}
	for _, a := range arcs {
		g.Arcs[a.ID] = a
	}
	return g, index.BuildAdjacency(g)
}

// Independent groups contend for lanes: a precedence-ready group may still be
// deferred purely by capacity, and the reason must name the lane holders.
func TestCapacityDeferralReason(t *testing.T) {
	vertices := []string{"a", "b", "c", "d"}
	// d depends on a; b and c are fully independent.
	arcs := []graph.Arc{
		{ID: "e1", Kind: "join", From: "a", To: "d", Cost: 1},
	}
	g, adj := buildGraph(t, vertices, arcs)
	res, err := Stages(g, adj, StagesRequest{
		Targets: []string{"a", "b", "c", "d"}, MaxParallel: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != "exact" || !res.Optimal {
		t.Fatalf("expected exact optimal, got method=%s optimal=%v", res.Method, res.Optimal)
	}
	// 4 groups, width 2, longest chain 2 -> lower bound 2.
	if res.LowerBound != 2 || res.UpperBound != 2 {
		t.Fatalf("bounds = %d/%d, want 2/2", res.LowerBound, res.UpperBound)
	}
	byGroup := map[string]WaitReason{}
	for _, r := range res.WaitReasons {
		byGroup[r.Group] = r
	}
	deferred := 0
	for id, r := range byGroup {
		if len(r.CapacityHeldBy) > 0 {
			deferred++
			if r.EarliestStage >= r.Stage {
				t.Fatalf("group %s not precedence-deferred but reports capacity: %+v", id, r)
			}
			if len(r.CapacityHeldBy) > 2 {
				t.Fatalf("capacity holders exceed width: %v", r.CapacityHeldBy)
			}
		}
	}
	if deferred == 0 {
		t.Fatalf("expected at least one capacity deferral, reasons=%+v", byGroup)
	}
}

// Closure must exclude unrelated groups even when they are graph vertices.
func TestClosureExcludesUnrelated(t *testing.T) {
	vertices := []string{"a", "b", "x"}
	arcs := []graph.Arc{
		{ID: "e1", Kind: "join", From: "a", To: "b", Cost: 1},
	}
	g, adj := buildGraph(t, vertices, arcs)
	res, err := Stages(g, adj, StagesRequest{
		Targets: []string{"b"}, MaxParallel: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range res.Included {
		if id == "x" {
			t.Fatalf("unrelated vertex x claimed a slot: %v", res.Included)
		}
	}
	if len(res.Included) != 2 {
		t.Fatalf("included=%v, want only a,b", res.Included)
	}
}

// Beyond the exact limit the answer is heuristic with tight, honest bounds.
func TestHeuristicBoundsForLargeClosure(t *testing.T) {
	n := exactGroupLimit + 5
	vertices := make([]string, n)
	arcs := []graph.Arc{}
	for i := range vertices {
		vertices[i] = "v" + string(rune('a'+i/26)) + string(rune('a'+i%26))
	}
	// Independent vertices: optimum is pure packing, which the greedy attains.
	g, adj := buildGraph(t, vertices, arcs)
	res, err := Stages(g, adj, StagesRequest{
		Targets: vertices, MaxParallel: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Method != "heuristic" || res.Optimal {
		t.Fatalf("expected heuristic non-optimal, got method=%s optimal=%v", res.Method, res.Optimal)
	}
	want := (n + 2) / 3
	if res.LowerBound != want || res.UpperBound != want {
		t.Fatalf("bounds=%d/%d want %d/%d", res.LowerBound, res.UpperBound, want, want)
	}
	// The plan itself must still be feasible and stage-packed.
	count := map[int]int{}
	for _, st := range res.Stages {
		count[st.Stage] = len(st.Groups)
		if len(st.Groups) > 3 {
			t.Fatalf("stage %d exceeds width", st.Stage)
		}
	}
	if len(count) != want {
		t.Fatalf("stage count=%d want %d", len(count), want)
	}
}

// Budget exhaustion surfaces as a budget error, never a partial plan.
func TestStagesBudgetExhaustion(t *testing.T) {
	vertices := []string{"a", "b", "c"}
	arcs := []graph.Arc{
		{ID: "e1", Kind: "join", From: "a", To: "b", Cost: 1},
		{ID: "e2", Kind: "join", From: "b", To: "c", Cost: 1},
	}
	g, adj := buildGraph(t, vertices, arcs)
	_, err := Stages(g, adj, StagesRequest{
		Targets: []string{"c"}, MaxParallel: 2,
		Selection: Selection{Budget: 1},
	})
	if err == nil {
		t.Fatal("expected budget error, got nil")
	}
	problem, ok := err.(*graph.Problem)
	if !ok || problem.Code != graph.Budget {
		t.Fatalf("want budget problem, got %v", err)
	}
}
