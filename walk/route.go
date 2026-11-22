package walk

import (
	"cmp"
	"slices"

	"rillweave/graph"
	"rillweave/index"
)

type RouteRequest struct {
	From      string          `json:"from"`
	To        string          `json:"to"`
	MaxHops   int             `json:"max_hops"`
	Pattern   Pattern         `json:"pattern"`
	Direction index.Direction `json:"direction,omitempty"`
	Budget    int             `json:"budget,omitempty"`
}

type RouteResult struct {
	Found    bool     `json:"found"`
	Cost     int64    `json:"cost"`
	Vertices []string `json:"vertices"`
	Arcs     []string `json:"arcs"`
	Scanned  int      `json:"scanned"`
}

// stage keys a search label by vertex and pattern stage: arriving at the same
// vertex in different stages of the pattern produces independent labels, so a
// route may revisit a vertex while the stage automaton advances.
type stage struct {
	vertex string
	state  int
}

func Route(g *graph.Graph, a *index.Adjacency, q RouteRequest) (RouteResult, error) {
	if err := hops(q.MaxHops); err != nil {
		return RouteResult{}, err
	}
	prog, err := compilePattern(g.Schema, q.Pattern)
	if err != nil {
		return RouteResult{}, err
	}
	t, err := topologyFor(g, a, Selection{Direction: q.Direction, Budget: q.Budget})
	if err != nil {
		return RouteResult{}, err
	}
	for _, v := range []string{q.From, q.To} {
		if err := requireVertex(g, v); err != nil {
			return RouteResult{}, err
		}
	}
	initial := label{vertices: []string{q.From}, arcs: []string{}}
	if q.From == q.To && prog.accept[prog.start] {
		return RouteResult{Found: true, Vertices: initial.vertices, Arcs: initial.arcs}, nil
	}
	current := map[stage]label{{q.From, prog.start}: initial}
	var best label
	found := false
	// Labels are layered by exact hop count; within one layer only the
	// cheapest stable prefix per stage is necessary, as in Path.
	for depth := 0; depth < q.MaxHops && len(current) != 0; depth++ {
		next := make(map[stage]label)
		for _, at := range sortedStages(current) {
			prefix := current[at]
			if found && prefix.cost >= best.cost {
				continue
			}
			if at.vertex == q.To && prog.accept[at.state] {
				continue // leaving a completed match only adds positive cost
			}
			err = t.visit(at.vertex, t.direction, func(arc graph.Arc, target string) error {
				state, ok := prog.step(at.state, arc.Kind)
				if !ok {
					return nil
				}
				cost := prefix.cost + arc.Cost
				key := stage{target, state}
				prior, seen := next[key]
				if seen && cost > prior.cost || found && cost > best.cost {
					return nil
				}
				candidate := label{
					cost: cost, arcs: append(slices.Clone(prefix.arcs), arc.ID),
					vertices: append(slices.Clone(prefix.vertices), target),
				}
				if !seen || better(candidate, prior) {
					next[key] = candidate
				}
				return nil
			})
			if err != nil {
				return RouteResult{}, err
			}
		}
		for at, candidate := range next {
			if at.vertex == q.To && prog.accept[at.state] && (!found || better(candidate, best)) {
				best, found = candidate, true
			}
		}
		current = next
	}
	if !found {
		return RouteResult{Vertices: []string{}, Arcs: []string{}, Scanned: t.scanned}, nil
	}
	return RouteResult{true, best.cost, best.vertices, best.arcs, t.scanned}, nil
}

func sortedStages(current map[stage]label) []stage {
	keys := make([]stage, 0, len(current))
	for at := range current {
		keys = append(keys, at)
	}
	slices.SortFunc(keys, func(a, b stage) int {
		if n := cmp.Compare(a.vertex, b.vertex); n != 0 {
			return n
		}
		return cmp.Compare(a.state, b.state)
	})
	return keys
}
