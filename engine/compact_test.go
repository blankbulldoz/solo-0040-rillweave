package engine

import (
	"testing"

	"rillweave/compress"
	"rillweave/graph"
	"rillweave/index"
)

func ringGraph() *graph.Graph {
	schema := graph.Schema{
		Vertices: map[string]graph.VertexRule{"node": {}},
		Arcs:     map[string]graph.ArcRule{"join": {From: "node", To: "node"}},
	}
	g := graph.New("ring", schema)
	for _, id := range []string{"r1", "r2", "r3", "r4", "sink"} {
		g.Vertices[id] = graph.Vertex{ID: id, Kind: "node"}
	}
	arcs := []graph.Arc{
		{ID: "r12", Kind: "join", From: "r1", To: "r2", Cost: 1},
		{ID: "r23", Kind: "join", From: "r2", To: "r3", Cost: 1},
		{ID: "r34", Kind: "join", From: "r3", To: "r4", Cost: 1},
		{ID: "r41", Kind: "join", From: "r4", To: "r1", Cost: 1},
		// One-way exit to a separate sink cluster: no path back, so r1->sink
		// is a cross-cluster pair with a definitive forward no-route only for
		// vertices that cannot reach r4 cheaply; here r4 reaches the sink.
		{ID: "exit", Kind: "join", From: "r4", To: "sink", Cost: 1},
	}
	for _, arc := range arcs {
		g.Arcs[arc.ID] = arc
	}
	return g
}

func TestCompressInstallAndReopen(t *testing.T) {
	root := t.TempDir()
	db, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	g := ringGraph()
	doc, err := compress.Build(g, index.BuildAdjacency(g), compress.Request{
		Name: "ring-mini", Kinds: []string{"join"}, Endpoints: []string{"r1", "r3", "sink"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	envelope, err := compress.Encode(doc)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	receipt, err := db.InstallCompressed(envelope)
	if err != nil {
		t.Fatalf("InstallCompressed: %v", err)
	}
	if receipt.Installed != "ring-mini" {
		t.Fatalf("unexpected receipt: %+v", receipt)
	}
	// Same-cluster endpoints are answered but visibly overview, never exact.
	inside, err := db.CompactRoute("ring-mini", compress.RouteRequest{From: "r1", To: "r3"})
	if err != nil {
		t.Fatalf("CompactRoute: %v", err)
	}
	if !inside.Result.Found || inside.Result.Exact || inside.Result.Cost != 2 {
		t.Fatalf("r1->r3 same-cluster overview mismatch: %+v", inside)
	}
	if !slicesEqual(inside.Result.Arcs, []string{"r12", "r23"}) {
		t.Fatalf("unexpected witness: %v", inside.Result.Arcs)
	}

	// Cross-cluster exact reachable pair: r1 -> sink around the ring exit.
	cross, err := db.CompactRoute("ring-mini", compress.RouteRequest{From: "r1", To: "sink"})
	if err != nil {
		t.Fatalf("CompactRoute: %v", err)
	}
	if !cross.Result.Found || !cross.Result.Exact || cross.Result.Cost != 4 {
		t.Fatalf("r1->sink exact route mismatch: %+v", cross)
	}
	if !slicesEqual(cross.Result.Arcs, []string{"r12", "r23", "r34", "exit"}) {
		t.Fatalf("unexpected cross witness: %v", cross.Result.Arcs)
	}

	// Cross-cluster exact definitive no-route: nothing ever leads back to r1.
	noRoute, err := db.CompactRoute("ring-mini", compress.RouteRequest{From: "sink", To: "r1"})
	if err != nil {
		t.Fatalf("CompactRoute: %v", err)
	}
	if noRoute.Result.Found || !noRoute.Result.Exact {
		t.Fatalf("sink->r1 must be exact no-route: %+v", noRoute)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen from disk without any in-memory state: the compressed head must
	// reconstruct and answer identically, with no source graph present.
	reopened, err := Open(root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	again, err := reopened.CompactRoute("ring-mini", compress.RouteRequest{From: "r1", To: "sink"})
	if err != nil {
		t.Fatalf("reopened route: %v", err)
	}
	if again.Result.Cost != 4 || !slicesEqual(again.Result.Arcs, cross.Result.Arcs) {
		t.Fatalf("reopened answer drifted: %+v", again)
	}
	verify, err := reopened.CompactReplay("ring-mini", again.Result.Arcs)
	if err != nil {
		t.Fatalf("reopened replay: %v", err)
	}
	if !verify.Result.Valid || verify.Result.Cost != 4 {
		t.Fatalf("offline replay failed: %+v", verify)
	}
	if _, err := reopened.CompactOverview("ring-mini"); err != nil {
		t.Fatalf("overview after reopen: %v", err)
	}
}

func TestInstallRejectsNameCollisionAndBadImage(t *testing.T) {
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	g := ringGraph()
	doc, err := compress.Build(g, index.BuildAdjacency(g), compress.Request{
		Name: "ring-mini", Endpoints: []string{"r1"},
	})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	envelope, err := compress.Encode(doc)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if _, err := db.InstallCompressed(envelope); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if _, err := db.InstallCompressed(envelope); err == nil {
		t.Fatal("reinstalling an occupied name must fail")
	}
	envelope.Digest = "0"
	if _, err := db.InstallCompressed(envelope); err == nil {
		t.Fatal("a tampered digest must be rejected")
	}
}

func slicesEqual(a, b []string) bool {
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
