package index

import (
	"errors"
	"testing"

	"rillweave/graph"
)

func TestCombineScratchGuard(t *testing.T) {
	g := graph.New("g", graph.Schema{
		Vertices: map[string]graph.VertexRule{
			"v": {Properties: map[string]graph.PropertyRule{"tag": {Kind: graph.Text}}},
		},
		Arcs: map[string]graph.ArcRule{"l": {From: "v", To: "v"}},
	})
	for i := 0; i < 1000; i++ {
		id := vertexID(i)
		g.Vertices[id] = graph.Vertex{ID: id, Kind: "v",
			Properties: map[string]graph.Scalar{"tag": {Kind: graph.Text, Text: "x"}}}
	}
	p := BuildPostings(g)
	terms := []Term{{Match: Match{Kind: "v", Property: "tag",
		Value: graph.Scalar{Kind: graph.Text, Text: "x"}}}}
	if _, err := p.Combine(terms, 100); err != nil {
		t.Fatalf("1000 vertices fit normal budget: %v", err)
	}
	// Force the guard by shrinking the allowance; the failure must surface
	// as the budget code so the HTTP layer maps it to 422.
	original := scratchLimit
	scratchLimit = 64
	defer func() { scratchLimit = original }()
	_, err := p.Combine(terms, 100)
	var problem *graph.Problem
	if !errors.As(err, &problem) || problem.Code != graph.Budget {
		t.Fatalf("want budget_exhausted problem, got %v", err)
	}
}
