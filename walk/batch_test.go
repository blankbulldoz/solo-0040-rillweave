package walk

import (
	"testing"

	"rillweave/graph"
	"rillweave/index"
)

// fixture is a hub with many outgoing arcs, cheap points, a filtered kind and a
// self-loop, so batch accounting can be compared with the single read.
func fixture(t *testing.T) (*graph.Graph, *index.Adjacency) {
	t.Helper()
	g := graph.New("fixture", graph.Schema{
		Vertices: map[string]graph.VertexRule{"strut": {}},
		Arcs: map[string]graph.ArcRule{
			"join":  {From: "strut", To: "strut"},
			"guide": {From: "strut", To: "strut"},
		},
	})
	for _, id := range []string{"hub", "near", "far"} {
		g.Vertices[id] = graph.Vertex{ID: id, Kind: "strut"}
	}
	add := func(id, from, to, kind string) {
		g.Arcs[id] = graph.Arc{ID: id, From: from, To: to, Kind: kind, Cost: 1}
	}
	add("a1", "hub", "far", "join")
	add("a2", "hub", "far", "join") // parallel arc keeps its own identity
	add("a3", "hub", "near", "join")
	add("a4", "hub", "hub", "join") // self-loop
	add("a5", "hub", "near", "guide")
	add("a6", "hub", "far", "guide")
	add("b1", "near", "hub", "join")
	add("c1", "far", "hub", "join")
	return g, index.BuildAdjacency(g)
}

type want struct {
	BatchQuery
	kinds []string
}

func TestBatchMatchesSingleRead(t *testing.T) {
	g, adj := fixture(t)
	cases := []want{
		{BatchQuery: BatchQuery{Vertex: "hub", Direction: index.Both}, kinds: []string{"join"}},
		{BatchQuery: BatchQuery{Vertex: "near"}, kinds: nil},
		{BatchQuery: BatchQuery{Vertex: "hub", Direction: index.In}, kinds: []string{"guide"}},
		{BatchQuery: BatchQuery{Vertex: "far", Direction: index.Both}, kinds: nil},
	}
	queries := make([]BatchQuery, len(cases))
	for n, c := range cases {
		queries[n] = c.BatchQuery
		queries[n].Kinds = c.kinds
	}
	batch, err := Batch(g, adj, BatchRequest{Queries: queries})
	if err != nil {
		t.Fatal(err)
	}
	if !batch.Complete {
		t.Fatal("batch with ample budget must complete every item")
	}
	for n, it := range batch.Items {
		single, err := Adjacent(g, adj, AdjacentRequest{
			Vertex:    it.Vertex,
			Selection: Selection{Direction: it.Direction, Kinds: cases[n].kinds},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(it.Arcs) != len(single.Arcs) {
			t.Fatalf("%s: %d arcs, single read had %d", it.Vertex, len(it.Arcs), len(single.Arcs))
		}
		for i := range it.Arcs {
			if it.Arcs[i].ID != single.Arcs[i].ID {
				t.Fatalf("%s: arc %d is %q, single read had %q", it.Vertex, i, it.Arcs[i].ID, single.Arcs[i].ID)
			}
		}
		if it.Matched != single.Matched || it.Scanned != single.Scanned {
			t.Fatalf("%s: matched/scanned %d/%d, single read had %d/%d",
				it.Vertex, it.Matched, it.Scanned, single.Matched, single.Scanned)
		}
	}
	hub := batch.Items[0]
	if hub.Matched != 6 || hub.Scanned != 8 {
		t.Fatalf("hub matched/scanned %d/%d, want 6/8", hub.Matched, hub.Scanned)
	}
	// Every returned arc is a selected kind, and the loop appears exactly once.
	ids := make(map[string]int)
	for _, a := range hub.Arcs {
		if a.Kind != "join" {
			t.Fatalf("hub returned filtered arc %q of kind %q", a.ID, a.Kind)
		}
		ids[a.ID]++
	}
	if len(ids) != 6 || ids["a4"] != 1 {
		t.Fatalf("hub arcs %v, want six distinct identities and one loop", ids)
	}
}

func TestBatchFairnessAndHonestTotals(t *testing.T) {
	g, adj := fixture(t)
	queries := []BatchQuery{
		{Vertex: "hub", Direction: index.Both},
		{Vertex: "near"},
		{Vertex: "far"},
	}
	// hub has 8 candidates in both directions; near and far have one outgoing
	// arc each. A page of 9 splits the guarantee three ways, so the cheap items
	// finish and the hub cannot finish with what is left.
	batch, err := Batch(g, adj, BatchRequest{Queries: queries, Budget: 9})
	if err != nil {
		t.Fatal(err)
	}
	if batch.Scanned != 9 {
		t.Fatalf("scanned %d, want the full page 9", batch.Scanned)
	}
	for _, it := range batch.Items[1:] {
		if !it.Complete || it.Matched != 1 {
			t.Fatalf("%s: complete=%v matched=%d, want complete with the exact total 1",
				it.Vertex, it.Complete, it.Matched)
		}
	}
	hub := batch.Items[0]
	if hub.Complete {
		t.Fatal("hub cannot finish in a page exhausted by other work")
	}
	if hub.Matched >= 8 {
		t.Fatalf("hub reported %d as its total while unfinished", hub.Matched)
	}
	if hub.Truncated {
		t.Fatal("an unfinished item must not claim a truncated-but-counted total")
	}
	if batch.Complete {
		t.Fatal("top-level complete must be false when any item is unfinished")
	}
}

func TestBatchFinishesWithinExactPage(t *testing.T) {
	g, adj := fixture(t)
	// Total work is hub 8 + near 1 + far 1 = 10.
	batch, err := Batch(g, adj, BatchRequest{
		Queries: []BatchQuery{
			{Vertex: "hub", Direction: index.Both},
			{Vertex: "near"},
			{Vertex: "far"},
		},
		Budget: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !batch.Complete || batch.Scanned != 10 {
		t.Fatalf("complete=%v scanned=%d, want complete after exactly 10", batch.Complete, batch.Scanned)
	}
}

func TestBatchRejectsUnworkablePage(t *testing.T) {
	g, adj := fixture(t)
	_, err := Batch(g, adj, BatchRequest{
		Queries: []BatchQuery{{Vertex: "hub"}, {Vertex: "near"}},
		Budget:  1,
	})
	if err == nil {
		t.Fatal("a page smaller than the query count must be refused")
	}
	if p, ok := err.(*graph.Problem); !ok || p.Code != graph.Invalid {
		t.Fatalf("got %v, want an invalid problem", err)
	}
}

func TestBatchRejectsBadSelection(t *testing.T) {
	g, adj := fixture(t)
	for _, q := range []BatchQuery{
		{Vertex: "hub", Direction: "sideways"},
		{Vertex: "hub", Kinds: []string{"unknown"}},
		{Vertex: "absent"},
	} {
		if _, err := Batch(g, adj, BatchRequest{Queries: []BatchQuery{q}}); err == nil {
			t.Fatalf("%+v was accepted", q)
		}
	}
}
