package index

import (
	"testing"

	"rillweave/graph"
)

func vertexID(i int) string {
	digits := []byte("00000")
	for p := len(digits) - 1; p >= 0; p-- {
		digits[p] = byte('0' + i%10)
		i /= 10
	}
	return "v" + string(digits)
}

func scalarInt(v int64) graph.Scalar   { return graph.Scalar{Kind: graph.Int, Int: v} }
func scalarBool(v bool) graph.Scalar   { return graph.Scalar{Kind: graph.Bool, Bool: v} }
func scalarText(v string) graph.Scalar { return graph.Scalar{Kind: graph.Text, Text: v} }

func ceilingSchema() graph.Schema {
	return graph.Schema{
		Vertices: map[string]graph.VertexRule{
			"v": {Properties: map[string]graph.PropertyRule{
				"code": {Kind: graph.Int},
				"flag": {Kind: graph.Bool},
			}},
		},
		Arcs: map[string]graph.ArcRule{"link": {From: "v", To: "v"}},
	}
}

func TestScratchCeiling(t *testing.T) {
	// Two dense bitmaps over the 10000-vertex ceiling must fit inside the
	// 128 KiB per-evaluation temporary-memory allowance.
	if got := scratchBytes(graph.MaxVertices); got > MaxScratch {
		t.Fatalf("scratch at %d vertices is %d bytes, limit is %d",
			graph.MaxVertices, got, MaxScratch)
	}
	// The guard stays meaningful: a graph an order of magnitude past the
	// real ceiling would trip it (threshold is just over 500k vertices).
	if scratchBytes(1000000) <= MaxScratch {
		t.Fatal("scratch guard never triggers; the budget check is unreachable")
	}
}

func TestCombineAtVertexCeiling(t *testing.T) {
	const n = graph.MaxVertices
	g := graph.New("bulk", ceilingSchema())
	for i := 0; i < n; i++ {
		id := vertexID(i)
		g.Vertices[id] = graph.Vertex{
			ID:   id,
			Kind: "v",
			Properties: map[string]graph.Scalar{
				"code": scalarInt(int64(i % 16)),
				"flag": scalarBool(i%2 == 0),
			},
		}
	}
	p := BuildPostings(g)

	// Union of all 16 codes covers every vertex exactly once despite overlap.
	terms := []Term{{Match: Match{Kind: "v", Property: "code", Value: scalarInt(0)}}}
	for code := 1; code < 16; code++ {
		terms = append(terms, Term{
			Match: Match{Kind: "v", Property: "code", Value: scalarInt(int64(code))},
			Op:    Union,
		})
	}
	post, err := p.Combine(terms, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if post.Matched != n {
		t.Fatalf("union matched %d, want %d", post.Matched, n)
	}
	if len(post.IDs) != 1000 || !post.Truncated {
		t.Fatalf("window = %d truncated=%v", len(post.IDs), post.Truncated)
	}
	for i := 1; i < len(post.IDs); i++ {
		if post.IDs[i-1] >= post.IDs[i] {
			t.Fatal("result window is not in sorted ID order")
		}
	}

	// Intersection with a rare posting keeps an exact, complete count.
	rare := append(terms, Term{
		Match: Match{Kind: "v", Property: "flag", Value: scalarBool(false)},
		Op:    Intersect,
	})
	post, err = p.Combine(rare, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if want := n / 2; post.Matched != want {
		t.Fatalf("intersection matched %d, want %d", post.Matched, want)
	}
	if !post.Truncated || len(post.IDs) != 1000 {
		t.Fatalf("5000 rows under a 1000 limit must be truncated, got %+v", post)
	}
}

func TestCombineDifferenceAndEmptyLeft(t *testing.T) {
	g := graph.New("g", graph.Schema{
		Vertices: map[string]graph.VertexRule{
			"v": {Properties: map[string]graph.PropertyRule{
				"tag":  {Kind: graph.Text},
				"code": {Kind: graph.Int},
			}},
		},
		Arcs: map[string]graph.ArcRule{"link": {From: "v", To: "v"}},
	})
	rows := []struct {
		id, tag string
		code    int64
	}{
		{"v1", "north", 1}, {"v2", "north", 2}, {"v3", "south", 1},
	}
	for _, row := range rows {
		g.Vertices[row.id] = graph.Vertex{
			ID: row.id, Kind: "v",
			Properties: map[string]graph.Scalar{
				"tag":  scalarText(row.tag),
				"code": scalarInt(row.code),
			},
		}
	}
	p := BuildPostings(g)

	post, err := p.Combine([]Term{
		{Match: Match{"v", "tag", scalarText("north")}},
		{Match: Match{"v", "code", scalarInt(2)}, Op: Subtract},
	}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if post.Matched != 1 || len(post.IDs) != 1 || post.IDs[0] != "v1" {
		t.Fatalf("difference = %+v", post)
	}

	// A missing property never matches: empty text returns no vertex.
	post, err = p.Combine([]Term{{Match: Match{"v", "tag", scalarText("")}}}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if post.Matched != 0 || len(post.IDs) != 0 || post.Truncated {
		t.Fatalf("missing property matched %+v", post)
	}

	// An already-empty left side still executes a further subtraction.
	post, err = p.Combine([]Term{
		{Match: Match{"v", "tag", scalarText("nowhere")}},
		{Match: Match{"v", "tag", scalarText("north")}, Op: Subtract},
	}, 100)
	if err != nil {
		t.Fatal(err)
	}
	if post.Matched != 0 {
		t.Fatalf("empty difference = %+v", post)
	}
}

func TestCombineRejects(t *testing.T) {
	p := BuildPostings(graph.New("g", graph.Schema{
		Vertices: map[string]graph.VertexRule{
			"v": {Properties: map[string]graph.PropertyRule{"tag": {Kind: graph.Text}}},
		},
		Arcs: map[string]graph.ArcRule{"link": {From: "v", To: "v"}},
	}))
	if _, err := p.Combine([]Term{
		{Match: Match{"v", "tag", scalarText("")}},
		{Match: Match{"v", "tag", scalarText("x")}, Op: "xor"},
	}, 100); err == nil {
		t.Fatal("unknown set operation must be rejected")
	}
}
