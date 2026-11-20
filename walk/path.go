package walk

import (
	"slices"

	"rillweave/graph"
	"rillweave/index"
)

type PathRequest struct {
	From    string `json:"from"`
	To      string `json:"to"`
	MaxHops int    `json:"max_hops"`
	Selection
}

type PathResult struct {
	Found    bool     `json:"found"`
	Cost     int64    `json:"cost"`
	Vertices []string `json:"vertices"`
	Arcs     []string `json:"arcs"`
	Scanned  int      `json:"scanned"`
}

type label struct {
	cost     int64
	arcs     []string
	vertices []string
}

func Path(g *graph.Graph, a *index.Adjacency, q PathRequest) (PathResult, error) {
	if err := hops(q.MaxHops); err != nil {
		return PathResult{}, err
	}
	t, err := topologyFor(g, a, q.Selection)
	if err != nil {
		return PathResult{}, err
	}
	for _, v := range []string{q.From, q.To} {
		if err := requireVertex(g, v); err != nil {
			return PathResult{}, err
		}
	}
	initial := label{vertices: []string{q.From}, arcs: []string{}}
	if q.From == q.To {
		return PathResult{Found: true, Vertices: initial.vertices, Arcs: initial.arcs}, nil
	}
	current := map[string]label{q.From: initial}
	var best label
	found := false
	// A cheaper prefix at a larger depth cannot dominate a shorter prefix.
	// Within each exact depth only the cheapest, stable prefix is necessary.
	for depth := 0; depth < q.MaxHops && len(current) != 0; depth++ {
		next := make(map[string]label)
		for _, vertex := range graph.Keys(current) {
			prefix := current[vertex]
			if vertex == q.To || found && prefix.cost >= best.cost {
				continue
			}
			err = t.visit(vertex, t.direction, func(arc graph.Arc, target string) error {
				cost := prefix.cost + arc.Cost
				prior, seen := next[target]
				if seen && cost > prior.cost || found && cost > best.cost {
					return nil
				}
				candidate := label{
					cost: cost, arcs: append(slices.Clone(prefix.arcs), arc.ID),
					vertices: append(slices.Clone(prefix.vertices), target),
				}
				if !seen || better(candidate, prior) {
					next[target] = candidate
				}
				return nil
			})
			if err != nil {
				return PathResult{}, err
			}
		}
		if candidate, ok := next[q.To]; ok && (!found || better(candidate, best)) {
			best, found = candidate, true
		}
		current = next
	}
	if !found {
		return PathResult{Vertices: []string{}, Arcs: []string{}, Scanned: t.scanned}, nil
	}
	return PathResult{true, best.cost, best.vertices, best.arcs, t.scanned}, nil
}

func better(a, b label) bool {
	if a.cost != b.cost {
		return a.cost < b.cost
	}
	if len(a.arcs) != len(b.arcs) {
		return len(a.arcs) < len(b.arcs)
	}
	return slices.Compare(a.arcs, b.arcs) < 0
}
