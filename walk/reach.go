package walk

import (
	"rillweave/graph"
	"rillweave/index"
)

type ReachRequest struct {
	Root    string `json:"root"`
	MaxHops int    `json:"max_hops"`
	Selection
}

type Distance struct {
	Vertex string `json:"vertex"`
	Hops   int    `json:"hops"`
}

type ReachResult struct {
	Distances []Distance `json:"distances"`
	Scanned   int        `json:"scanned"`
}

func Reach(g *graph.Graph, a *index.Adjacency, q ReachRequest) (ReachResult, error) {
	if err := hops(q.MaxHops); err != nil {
		return ReachResult{}, err
	}
	t, err := topologyFor(g, a, q.Selection)
	if err != nil {
		return ReachResult{}, err
	}
	if err := requireVertex(g, q.Root); err != nil {
		return ReachResult{}, err
	}
	result := ReachResult{Distances: []Distance{{q.Root, 0}}}
	seen := map[string]bool{q.Root: true}
	for i := 0; i < len(result.Distances); i++ {
		current := result.Distances[i]
		if current.Hops == q.MaxHops {
			break
		}
		err := t.visit(current.Vertex, t.direction, func(_ graph.Arc, next string) error {
			if !seen[next] {
				seen[next] = true
				result.Distances = append(result.Distances, Distance{next, current.Hops + 1})
			}
			return nil
		})
		if err != nil {
			return ReachResult{}, err
		}
	}
	result.Scanned = t.scanned
	return result, nil
}

type AdjacentRequest struct {
	Vertex string `json:"vertex"`
	Limit  int    `json:"limit,omitempty"`
	// Cursor continues a previous page. An empty value starts a new scan.
	Cursor string `json:"cursor,omitempty"`
	Selection
}

type AdjacentResult struct {
	Arcs      []graph.Arc `json:"arcs"`
	Matched   int         `json:"matched"`
	Truncated bool        `json:"truncated"`
	Scanned   int         `json:"scanned"`
	// NextCursor resumes after the last returned arc. It is absent once every
	// matching arc has been delivered.
	NextCursor string `json:"next_cursor,omitempty"`
}

// Adjacent lists one bounded window of incidence in arc ID order. A window is
// resumed with the opaque cursor rather than restarted, so earlier pages are
// neither re-scanned nor re-billed. The cursor names the revision that owns
// its position; reading against another revision is rejected as a conflict.
func Adjacent(g *graph.Graph, a *index.Adjacency, q AdjacentRequest) (AdjacentResult, error) {
	limit, err := Limit(q.Limit)
	if err != nil {
		return AdjacentResult{}, err
	}
	selection := q.Selection
	token := pageToken{}
	if q.Cursor != "" {
		token, err = decodeToken(q.Cursor)
		if err != nil {
			return AdjacentResult{}, err
		}
		// A cursor continues its own logical scan; the request parameters must
		// describe the same scan or the token cannot be interpreted.
		if token.Vertex != q.Vertex || token.Direction != selection.Direction ||
			!sameKinds(token.Kinds, canonicalKinds(selection.Kinds)) {
			return AdjacentResult{}, graph.Fail(graph.Invalid, "cursor does not match the requested vertex, direction or kinds")
		}
		if token.Revision != g.Revision {
			return AdjacentResult{}, graph.Fail(graph.Conflict,
				"cursor was issued at revision %d but the selected view is now at revision %d; restart paging or hold the old revision with a pin",
				token.Revision, g.Revision)
		}
		if token.Offset < 0 {
			return AdjacentResult{}, graph.Fail(graph.Invalid, "cursor carries an invalid offset")
		}
	}
	t, err := topologyFor(g, a, selection)
	if err != nil {
		return AdjacentResult{}, err
	}
	if err := requireVertex(g, q.Vertex); err != nil {
		return AdjacentResult{}, err
	}

	result := AdjacentResult{Arcs: []graph.Arc{}}
	err = t.scanFrom(q.Vertex, t.direction, token.After, limit, func(arc graph.Arc, _ string) error {
		result.Arcs = append(result.Arcs, arc)
		return nil
	})
	if err != nil {
		return AdjacentResult{}, err
	}

	// Matched is the exact total in the selected revision, independent of the
	// window position, read from incidence tallies rather than a full scan.
	result.Matched = a.Count(q.Vertex, t.direction, t.kinds)
	result.Scanned = t.scanned
	offset := token.Offset + len(result.Arcs)
	result.Truncated = offset < result.Matched
	if result.Truncated {
		result.NextCursor = encodeToken(pageToken{
			Vertex:    q.Vertex,
			Direction: t.direction,
			Kinds:     canonicalKinds(selection.Kinds),
			Revision:  g.Revision,
			After:     result.Arcs[len(result.Arcs)-1].ID,
			Offset:    offset,
		})
	}
	return result, nil
}

func Limit(request int) (int, error) {
	if request == 0 {
		return 100, nil
	}
	if request < 1 || request > 1000 {
		return 0, graph.Fail(graph.Invalid, "limit must be in [1,1000]")
	}
	return request, nil
}
