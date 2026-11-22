package walk

import (
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
	start := label{witness: witnessAt(q.From)}
	if q.From == q.To {
		return report(start, t.scanned), nil
	}
	best, found, err := search(t, q, start)
	if err != nil {
		return PathResult{}, err
	}
	if !found {
		return PathResult{Vertices: []string{}, Arcs: []string{}, Scanned: t.scanned}, nil
	}
	return report(best, t.scanned), nil
}

// search runs exact-depth relaxation. A cheaper prefix at a larger depth cannot
// dominate a shorter prefix, so each depth keeps only the stable winner per
// vertex. The incumbent best witness is compared purely through prefer.
func search(t *topology, q PathRequest, start label) (label, bool, error) {
	current := frontier{q.From: start}
	var best label
	found := false
	for depth := 0; depth < q.MaxHops && len(current) != 0; depth++ {
		next := make(frontier)
		for _, vertex := range graph.Keys(current) {
			prefix := current[vertex]
			if vertex == q.To || found && prefix.cost >= best.cost {
				continue
			}
			err := t.visit(vertex, t.direction, func(arc graph.Arc, target string) error {
				cost := prefix.cost + arc.Cost
				if found && cost > best.cost {
					return nil
				}
				next.admit(target, label{cost: cost, witness: prefix.witness.extend(arc, target)})
				return nil
			})
			if err != nil {
				return label{}, false, err
			}
		}
		if candidate, ok := next[q.To]; ok && (!found || prefer(candidate, best)) {
			best, found = candidate, true
		}
		current = next
	}
	return best, found, nil
}

func report(best label, scanned int) PathResult {
	return PathResult{true, best.cost, best.witness.vertices(), best.witness.arcs(), scanned}
}
