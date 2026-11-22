package walk

import (
	"cmp"
	"slices"

	"rillweave/graph"
	"rillweave/index"
)

type PathRequest struct {
	From    string   `json:"from"`
	To      string   `json:"to"`
	Via     []string `json:"via,omitempty"`
	MaxHops int      `json:"max_hops"`
	Selection
}

type PathResult struct {
	Found    bool     `json:"found"`
	Cost     int64    `json:"cost"`
	Vertices []string `json:"vertices"`
	Arcs     []string `json:"arcs"`
	ViaHops  []int    `json:"via_hops"`
	Scanned  int      `json:"scanned"`
}

// A stage pins a walk position to the number of via waypoints satisfied so
// far. One position satisfies at most one waypoint, so a repeated waypoint
// needs a distinct later visit and passing a waypoint before its turn counts
// for nothing.
type stage struct {
	vertex string
	step   int
}

type label struct {
	cost     int64
	arcs     []string
	vertices []string
	viaHops  []int
}

func Path(g *graph.Graph, a *index.Adjacency, q PathRequest) (PathResult, error) {
	if err := hops(q.MaxHops); err != nil {
		return PathResult{}, err
	}
	if len(q.Via) > MaxVia {
		return PathResult{}, graph.Fail(graph.Invalid, "via accepts at most %d ordered waypoints", MaxVia)
	}
	t, err := topologyFor(g, a, q.Selection)
	if err != nil {
		return PathResult{}, err
	}
	for _, v := range append(slices.Clone(q.Via), q.From, q.To) {
		if err := requireVertex(g, v); err != nil {
			return PathResult{}, err
		}
	}
	initial := label{vertices: []string{q.From}, arcs: []string{}, viaHops: []int{}}
	start := stage{vertex: q.From}
	if len(q.Via) != 0 && q.Via[0] == q.From {
		start.step = 1
		initial.viaHops = []int{0}
	}
	goal := stage{vertex: q.To, step: len(q.Via)}
	current := map[stage]label{start: initial}
	var best label
	found := false
	if start == goal {
		best, found = initial, true
	}
	// A cheaper prefix at a larger depth cannot dominate a shorter prefix.
	// Within each exact depth only the cheapest, stable prefix per stage is
	// necessary, and positive arc costs make any label at or above the best
	// goal cost useless.
	for depth := 0; depth < q.MaxHops && len(current) != 0; depth++ {
		next := make(map[stage]label)
		for _, s := range sortedStages(current) {
			prefix := current[s]
			if s == goal || found && prefix.cost >= best.cost {
				continue
			}
			err = t.visit(s.vertex, t.direction, func(arc graph.Arc, target string) error {
				cost := prefix.cost + arc.Cost
				if found && cost > best.cost {
					return nil
				}
				key := stage{vertex: target, step: s.step}
				advance := s.step < len(q.Via) && target == q.Via[s.step]
				if advance {
					key.step++
				}
				prior, seen := next[key]
				if seen && cost > prior.cost {
					return nil
				}
				candidate := label{
					cost:     cost,
					arcs:     append(slices.Clone(prefix.arcs), arc.ID),
					vertices: append(slices.Clone(prefix.vertices), target),
					viaHops:  slices.Clone(prefix.viaHops),
				}
				if advance {
					candidate.viaHops = append(candidate.viaHops, depth+1)
				}
				if !seen || better(candidate, prior) {
					next[key] = candidate
				}
				return nil
			})
			if err != nil {
				return PathResult{}, err
			}
		}
		if candidate, ok := next[goal]; ok && (!found || better(candidate, best)) {
			best, found = candidate, true
		}
		current = next
	}
	if !found {
		return PathResult{Vertices: []string{}, Arcs: []string{}, ViaHops: []int{}, Scanned: t.scanned}, nil
	}
	return PathResult{true, best.cost, best.vertices, best.arcs, best.viaHops, t.scanned}, nil
}

func sortedStages(m map[stage]label) []stage {
	keys := make([]stage, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b stage) int {
		if n := cmp.Compare(a.vertex, b.vertex); n != 0 {
			return n
		}
		return cmp.Compare(a.step, b.step)
	})
	return keys
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
