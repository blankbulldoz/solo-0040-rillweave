package walk

import (
	"testing"

	"rillweave/graph"
	"rillweave/index"
)

// unevenGraph builds a graph whose first frontier row is wide and whose
// degrees vary sharply, so paging must pause in the middle of adjacency rows
// without ever treating a layer as complete early.
func unevenGraph() (*graph.Graph, *index.Adjacency) {
	g := graph.New("probe", graph.Schema{
		Vertices: map[string]graph.VertexRule{"strut": {}},
		Arcs:     map[string]graph.ArcRule{"join": {From: "strut", To: "strut"}},
	})
	ids := []string{"root", "wide1", "wide2", "wide3", "deep", "leaf1", "leaf2", "leaf3", "sink"}
	for _, id := range ids {
		g.Vertices[id] = graph.Vertex{ID: id, Kind: "strut"}
	}
	arcs := []graph.Arc{
		{ID: "a01", Kind: "join", From: "root", To: "wide1", Cost: 1},
		{ID: "a02", Kind: "join", From: "root", To: "wide2", Cost: 1},
		{ID: "a03", Kind: "join", From: "root", To: "wide3", Cost: 1},
		{ID: "a04", Kind: "join", From: "root", To: "deep", Cost: 1},
		{ID: "a10", Kind: "join", From: "wide1", To: "sink", Cost: 1},
		{ID: "a20", Kind: "join", From: "wide2", To: "sink", Cost: 1},
		{ID: "a30", Kind: "join", From: "wide3", To: "sink", Cost: 1},
		{ID: "ad1", Kind: "join", From: "deep", To: "leaf1", Cost: 1},
		{ID: "ad2", Kind: "join", From: "deep", To: "leaf2", Cost: 1},
		{ID: "ad3", Kind: "join", From: "deep", To: "leaf3", Cost: 1},
		{ID: "a11", Kind: "join", From: "leaf1", To: "sink", Cost: 1},
		{ID: "a22", Kind: "join", From: "leaf2", To: "sink", Cost: 1},
		{ID: "a33", Kind: "join", From: "leaf3", To: "sink", Cost: 1},
		{ID: "loop", Kind: "join", From: "sink", To: "sink", Cost: 1},
	}
	for _, arc := range arcs {
		g.Arcs[arc.ID] = arc
	}
	return g, index.BuildAdjacency(g)
}

func TestReachCursorMatchesFullReach(t *testing.T) {
	g, adj := unevenGraph()
	full, err := Reach(g, adj, ReachRequest{Root: "root", MaxHops: 4})
	if err != nil {
		t.Fatalf("full reach: %v", err)
	}
	for _, pageBudget := range []int{1, 2, 3, 5, 1000} {
		cursor, err := NewReachCursor(g, adj, ReachPageRequest{
			Root: "root", MaxHops: 4, Selection: Selection{Budget: 1000},
		})
		if err != nil {
			t.Fatalf("open cursor: %v", err)
		}
		var stitched []Distance
		pages, totalScanned := 0, 0
		for {
			page, err := cursor.Advance(g, adj, pageBudget)
			if err != nil {
				t.Fatalf("advance budget=%d: %v", pageBudget, err)
			}
			stitched = append(stitched, page.Distances...)
			totalScanned += page.Scanned
			pages++
			if page.Complete {
				break
			}
			if pages > 100 {
				t.Fatalf("budget=%d never completed", pageBudget)
			}
		}
		if !cursor.Done() {
			t.Fatalf("budget=%d cursor not done", pageBudget)
		}
		if len(stitched) != len(full.Distances) {
			t.Fatalf("budget=%d stitched %d rows, full reach %d", pageBudget, len(stitched), len(full.Distances))
		}
		for i, want := range full.Distances {
			if stitched[i] != want {
				t.Fatalf("budget=%d row %d = %+v, want %+v", pageBudget, i, stitched[i], want)
			}
		}
		if totalScanned != full.Scanned {
			t.Fatalf("budget=%d scanned %d arcs, full reach %d", pageBudget, totalScanned, full.Scanned)
		}
		// One arc per page: one root-only page plus one page per inspection,
		// including the final empty page that marks the exhausted frontier.
		if pageBudget == 1 && pages != full.Scanned+2 {
			t.Fatalf("one-arc pages: got %d pages, want %d", pages, full.Scanned+2)
		}
	}
}

func TestReachCursorLayerDoesNotCloseMidRow(t *testing.T) {
	g, adj := unevenGraph()
	// One arc per page. The opening page carries only the root (no budget is
	// spent on it); the root's four-arc row then needs four one-arc pages.
	cursor, err := NewReachCursor(g, adj, ReachPageRequest{
		Root: "root", MaxHops: 4, Selection: Selection{Budget: 1000},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	opening, err := cursor.Advance(g, adj, 1)
	if err != nil {
		t.Fatalf("opening page: %v", err)
	}
	if opening.Distances[0] != (Distance{Vertex: "root", Hops: 0}) {
		t.Fatalf("opening page missing root: %+v", opening)
	}
	// Drive with one-arc pages. While the root row is still open, every newly
	// proven fact must be a hop-1 vertex; a hop-2 fact would mean a frontier
	// row was treated as complete before its scan ended.
	var layerOne []Distance
	rootScans := 0
	for rootScans < 4 {
		if cursor.queue[0].vertex != "root" {
			t.Fatalf("root row left the queue head after %d scans", rootScans)
		}
		page, err := cursor.Advance(g, adj, 1)
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		if page.Complete {
			t.Fatal("completed with an unscanned frontier row")
		}
		rootScans += page.Scanned
		for _, d := range page.Distances {
			if d.Hops != 1 {
				t.Fatalf("delivered hop %d before root row closed: %+v", d.Hops, d)
			}
			layerOne = append(layerOne, d)
		}
	}
	if len(layerOne) != 4 {
		t.Fatalf("layer one = %+v, want four vertices", layerOne)
	}
	// The root row is fully scanned; the queue head is the first layer-1 row.
	if cursor.queue[0].vertex != "root" || len(cursor.queue) != 5 {
		t.Fatalf("queue state after root row: %+v", cursor.queue)
	}
	next, err := cursor.Advance(g, adj, 1)
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if next.Complete {
		t.Fatal("frontier still contains the layer-1 rows")
	}
	for _, d := range next.Distances {
		if d.Hops != 2 {
			t.Fatalf("layer 2 leaked hop %d: %+v", d.Hops, d)
		}
	}
}

func TestReachCursorHopBoundLeavesFrontierComplete(t *testing.T) {
	g, adj := unevenGraph()
	cursor, err := NewReachCursor(g, adj, ReachPageRequest{
		Root: "root", MaxHops: 1, Selection: Selection{Budget: 1},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	var rows []Distance
	for {
		page, err := cursor.Advance(g, adj, 1)
		if err != nil {
			t.Fatalf("advance: %v", err)
		}
		rows = append(rows, page.Distances...)
		if page.Complete {
			break
		}
	}
	want := map[string]int{"root": 0, "wide1": 1, "wide2": 1, "wide3": 1, "deep": 1}
	got := map[string]int{}
	for _, d := range rows {
		got[d.Vertex] = d.Hops
	}
	if len(got) != len(want) {
		t.Fatalf("hop-bound rows = %v, want %v", got, want)
	}
	for v, h := range want {
		if got[v] != h {
			t.Fatalf("vertex %q hops %d, want %d", v, got[v], h)
		}
	}
}

func TestReachCursorRejectsBadPageBudget(t *testing.T) {
	g, adj := unevenGraph()
	cursor, err := NewReachCursor(g, adj, ReachPageRequest{
		Root: "root", MaxHops: 2, Selection: Selection{Budget: 10},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := cursor.Advance(g, adj, MaxBudget+1); err == nil {
		t.Fatal("budget above cap was accepted")
	}
	if _, err := cursor.Advance(g, adj, -1); err == nil {
		t.Fatal("negative budget was accepted")
	}
}

func TestReachCursorSelfLoopVisitedOnceAcrossWindows(t *testing.T) {
	g, adj := unevenGraph()
	cursor, err := NewReachCursor(g, adj, ReachPageRequest{
		Root: "sink", MaxHops: 2, Selection: Selection{Budget: 1},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// A fresh cursor first returns the root; the root costs no inspection, so
	// a one-arc page ends at the self loop without scanning it.
	first, err := cursor.Advance(g, adj, 1)
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if len(first.Distances) != 1 || first.Distances[0].Vertex != "sink" {
		t.Fatalf("opening page = %+v, want only the root", first.Distances)
	}
	if first.Complete {
		t.Fatal("opening page cannot complete before the root row is scanned")
	}
	second, err := cursor.Advance(g, adj, 1)
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	if len(second.Distances) != 0 {
		t.Fatalf("self loop produced a distance: %+v", second.Distances)
	}
	if second.Scanned != 1 {
		t.Fatalf("self-loop inspection count = %d, want 1", second.Scanned)
	}
	// The bound layer is consumed without further inspections; after that the
	// frontier is exhausted and completion is explicit.
	third, err := cursor.Advance(g, adj, 1)
	if err != nil {
		t.Fatalf("third page: %v", err)
	}
	if third.Scanned != 0 || !third.Complete {
		t.Fatalf("frontier should finish with no further inspection: %+v", third)
	}
}

func TestVisitWindowMatchesVisit(t *testing.T) {
	_, adj := unevenGraph()
	var full []string
	err := adj.Visit("root", index.Both, func(a graph.Arc, _ string) error {
		full = append(full, a.ID)
		return nil
	})
	if err != nil {
		t.Fatalf("visit: %v", err)
	}
	var windowed []string
	outOff, inOff := 0, 0
	for len(windowed) < len(full) {
		var batch []string
		nextOut, nextIn, err := adj.VisitWindow("root", index.Both, outOff, inOff,
			func(graph.Arc, string) (bool, error) { return len(batch) == 2, nil },
			func(a graph.Arc, _ string) error { batch = append(batch, a.ID); return nil },
		)
		if err != nil {
			t.Fatalf("window: %v", err)
		}
		if nextOut == outOff && nextIn == inOff {
			t.Fatal("window made no progress")
		}
		windowed = append(windowed, batch...)
		outOff, inOff = nextOut, nextIn
	}
	if len(windowed) != len(full) {
		t.Fatalf("window %v, visit %v", windowed, full)
	}
	for i := range full {
		if windowed[i] != full[i] {
			t.Fatalf("position %d window %q, visit %q", i, windowed[i], full[i])
		}
	}
}
