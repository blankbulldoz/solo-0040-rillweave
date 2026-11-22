package walk

import (
	"testing"

	"rillweave/graph"
	"rillweave/index"
)

func testGraph() *graph.Graph {
	g := graph.New("g", graph.Schema{
		Vertices: map[string]graph.VertexRule{"s": {}},
		Arcs: map[string]graph.ArcRule{
			"a": {From: "s", To: "s"},
			"b": {From: "s", To: "s"},
		},
	})
	ids := []string{"s", "t", "u", "v", "x"}
	for _, id := range ids {
		g.Vertices[id] = graph.Vertex{ID: id, Kind: "s"}
	}
	// s -> t (two parallel arcs), s -> x -> t (dead branch alternative that
	// cannot reach t), t -> u -> v target, u -> t cycle, self loop on s.
	arcs := []graph.Arc{
		{ID: "e1", Kind: "a", From: "s", To: "t", Cost: 5},
		{ID: "e2", Kind: "a", From: "s", To: "t", Cost: 2},
		{ID: "dead", Kind: "a", From: "s", To: "x", Cost: 1},
		{ID: "loop", Kind: "a", From: "s", To: "s", Cost: 1},
		{ID: "tu", Kind: "a", From: "t", To: "u", Cost: 2},
		{ID: "uv", Kind: "a", From: "u", To: "v", Cost: 2},
		{ID: "ut", Kind: "b", From: "u", To: "t", Cost: 1},
	}
	for _, arc := range arcs {
		g.Arcs[arc.ID] = arc
	}
	return g
}

// oracle enumerates every walk of at most H hops from source to target and
// returns the arc IDs that appear in at least one walk whose total cost is
// within C, together with one witnessing walk each.
func oracle(g *graph.Graph, dir index.Direction, kinds map[string]bool, from, to string, H int, C int64) map[string][]string {
	type state struct {
		vertex string
		hops   int
		cost   int64
		arcs   []string
	}
	witness := map[string][]string{}
	queue := []state{{vertex: from}}
	for len(queue) != 0 {
		cur := queue[0]
		queue = queue[1:]
		if cur.vertex == to {
			for _, id := range cur.arcs {
				if _, ok := witness[id]; !ok {
					witness[id] = append([]string(nil), cur.arcs...)
				}
			}
		}
		if cur.hops == H {
			continue
		}
		for _, arc := range g.Arcs {
			if !kinds[arc.Kind] {
				continue
			}
			var src, dst string
			switch dir {
			case index.Out:
				src, dst = arc.From, arc.To
			case index.In:
				src, dst = arc.To, arc.From
			case index.Both:
				if arc.From == cur.vertex {
					src, dst = arc.From, arc.To
				} else if arc.To == cur.vertex {
					src, dst = arc.To, arc.From
				} else {
					continue
				}
			}
			if src != cur.vertex || cur.cost+arc.Cost > C {
				continue
			}
			next := state{vertex: dst, hops: cur.hops + 1, cost: cur.cost + arc.Cost}
			next.arcs = append(append([]string(nil), cur.arcs...), arc.ID)
			queue = append(queue, next)
		}
	}
	return witness
}

func replayWitness(t *testing.T, g *graph.Graph, dir index.Direction, from, to string, H int, C int64, seq []string) {
	t.Helper()
	if len(seq) > H {
		t.Fatalf("witness %v exceeds hop limit %d", seq, H)
	}
	vertex, cost := from, int64(0)
	for _, id := range seq {
		arc := g.Arcs[id]
		src, dst := arc.From, arc.To
		if dir == index.In {
			src, dst = arc.To, arc.From
		}
		// In bidirectional selection one walk may mix orientations; infer the
		// step that actually leaves the current vertex. A self-loop fits either.
		if dir == index.Both && src != vertex {
			src, dst = arc.To, arc.From
		}
		if src != vertex {
			t.Fatalf("witness %v breaks at arc %s: at %s, edge leaves %s", seq, id, vertex, src)
		}
		vertex, cost = dst, cost+arc.Cost
	}
	if vertex != to {
		t.Fatalf("witness %v ends at %s, want %s", seq, vertex, to)
	}
	if cost > C {
		t.Fatalf("witness %v costs %d, exceeds %d", seq, cost, C)
	}
}

func TestCorridorMatchesOracle(t *testing.T) {
	g := testGraph()
	adj := index.BuildAdjacency(g)
	kinds := map[string]bool{"a": true, "b": true}
	cases := []struct {
		dir       index.Direction
		from, to  string
		H         int
		C         int64
		selection Selection
	}{
		{index.Out, "s", "v", 4, 20, Selection{}},
		{index.Out, "s", "v", 2, 20, Selection{}},
		{index.Out, "s", "v", 4, 8, Selection{}}, // splice trap region
		{index.Out, "s", "t", 3, 3, Selection{}}, // repeated vertex via self loop
		{index.Out, "s", "s", 2, 2, Selection{}}, // same endpoints
		{index.In, "v", "s", 4, 20, Selection{}}, // reversed
		{index.In, "v", "s", 2, 9, Selection{}},
		{index.Both, "x", "v", 4, 20, Selection{}}, // bidirectional escape
		{index.Both, "s", "v", 3, 6, Selection{}},
	}
	for _, tc := range cases {
		tc.selection.Direction = tc.dir
		want := oracle(g, tc.dir, kinds, tc.from, tc.to, tc.H, tc.C)
		got, err := Corridor(g, adj, CorridorRequest{
			From: tc.from, To: tc.to, MaxHops: tc.H, MaxCost: tc.C,
			Samples: 100, Selection: tc.selection,
		})
		if err != nil {
			t.Fatalf("corridor %+v: %v", tc, err)
		}
		if got.Status != StatusComplete {
			t.Fatalf("corridor %+v: status %s", tc, got.Status)
		}
		gotSet := map[string]bool{}
		for _, row := range got.Included {
			gotSet[row.Arc] = true
			if _, ok := want[row.Arc]; !ok {
				t.Fatalf("corridor %+v: spurious arc %s", tc, row.Arc)
			}
			replayWitness(t, g, tc.dir, tc.from, tc.to, tc.H, tc.C, row.Witness)
			if len(row.Witness) == 0 {
				t.Fatalf("corridor %+v: included arc %s lacks a witness", tc, row.Arc)
			}
		}
		for id := range want {
			if !gotSet[id] {
				t.Fatalf("corridor %+v: missing arc %s", tc, id)
			}
		}
		excluded := map[string]bool{}
		for _, id := range got.Excluded {
			excluded[id] = true
		}
		for id, arc := range g.Arcs {
			if !kinds[arc.Kind] {
				continue
			}
			if !gotSet[id] && !excluded[id] {
				t.Fatalf("corridor %+v: arc %s neither included nor excluded", tc, id)
			}
			if gotSet[id] && excluded[id] {
				t.Fatalf("corridor %+v: arc %s both included and excluded", tc, id)
			}
		}
	}
}

func TestCorridorTruncationSoundness(t *testing.T) {
	g := testGraph()
	adj := index.BuildAdjacency(g)
	// A budget smaller than the selected candidate count forces truncation.
	got, err := Corridor(g, adj, CorridorRequest{
		From: "s", To: "v", MaxHops: 4, MaxCost: 20, Samples: 100,
		Selection: Selection{Budget: 3},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusTruncated || len(got.Excluded) != 0 {
		t.Fatalf("truncation must forbid exclusions: %+v", got)
	}
	full, err := Corridor(g, adj, CorridorRequest{
		From: "s", To: "v", MaxHops: 4, MaxCost: 20, Samples: 100,
		Selection: Selection{},
	})
	if err != nil {
		t.Fatal(err)
	}
	fullIncluded := map[string]bool{}
	for _, row := range full.Included {
		fullIncluded[row.Arc] = true
	}
	for _, row := range got.Included {
		if !fullIncluded[row.Arc] {
			t.Fatalf("truncated run proved an arc the full run excludes: %s", row.Arc)
		}
		replayWitness(t, g, index.Out, "s", "v", 4, 20, row.Witness)
	}
	decided := map[string]bool{}
	for _, row := range got.Included {
		decided[row.Arc] = true
	}
	for _, id := range got.Undecided {
		decided[id] = true
	}
	for id := range g.Arcs {
		if !decided[id] {
			t.Fatalf("selected arc %s missing from truncated verdict", id)
		}
	}
	if got.Scanned != 3 {
		t.Fatalf("scanned = %d, want 3", got.Scanned)
	}
}

func TestCorridorRejectsInvalidInput(t *testing.T) {
	g := testGraph()
	adj := index.BuildAdjacency(g)
	base := CorridorRequest{From: "s", To: "v", MaxHops: 2, MaxCost: 5}
	if _, err := Corridor(g, adj, base); err != nil {
		t.Fatalf("base request should be valid: %v", err)
	}
	bad := base
	bad.MaxCost = 0
	if _, err := Corridor(g, adj, bad); err == nil {
		t.Fatal("non-positive max_cost must be rejected")
	}
	bad = base
	bad.MaxHops = MaxHops + 1
	if _, err := Corridor(g, adj, bad); err == nil {
		t.Fatal("out-of-range max_hops must be rejected")
	}
	bad = base
	bad.From = "ghost"
	if _, err := Corridor(g, adj, bad); err == nil {
		t.Fatal("missing endpoint must be rejected")
	}
	bad = base
	bad.Kinds = []string{"unknown"}
	if _, err := Corridor(g, adj, bad); err == nil {
		t.Fatal("unknown kind must be rejected")
	}
}

func TestCorridorSamplesAreBounded(t *testing.T) {
	g := testGraph()
	adj := index.BuildAdjacency(g)
	got, err := Corridor(g, adj, CorridorRequest{
		From: "s", To: "v", MaxHops: 4, MaxCost: 20, Samples: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Sampled != 2 {
		t.Fatalf("sampled = %d, want 2", got.Sampled)
	}
	withWitness := 0
	for _, row := range got.Included {
		if len(row.Witness) != 0 {
			withWitness++
		}
	}
	if withWitness != 2 {
		t.Fatalf("witnessed rows = %d, want 2", withWitness)
	}
}
