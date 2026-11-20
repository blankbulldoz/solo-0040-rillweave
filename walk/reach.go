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
	Selection
}

type AdjacentResult struct {
	Arcs      []graph.Arc `json:"arcs"`
	Matched   int         `json:"matched"`
	Truncated bool        `json:"truncated"`
	Scanned   int         `json:"scanned"`
}

func Adjacent(g *graph.Graph, a *index.Adjacency, q AdjacentRequest) (AdjacentResult, error) {
	limit, err := Limit(q.Limit)
	if err != nil {
		return AdjacentResult{}, err
	}
	t, err := topologyFor(g, a, q.Selection)
	if err != nil {
		return AdjacentResult{}, err
	}
	if err := requireVertex(g, q.Vertex); err != nil {
		return AdjacentResult{}, err
	}
	result := AdjacentResult{Arcs: []graph.Arc{}}
	err = t.visit(q.Vertex, t.direction, func(arc graph.Arc, _ string) error {
		result.Matched++
		if len(result.Arcs) < limit {
			result.Arcs = append(result.Arcs, arc)
		}
		return nil
	})
	if err != nil {
		return AdjacentResult{}, err
	}
	result.Scanned = t.scanned
	result.Truncated = result.Matched > limit
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
