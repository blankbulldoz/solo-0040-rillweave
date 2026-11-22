package walk

import (
	"rillweave/graph"
	"rillweave/index"
)

const (
	MaxHops   = 32
	MaxBudget = 300000
)

type Selection struct {
	Kinds     []string        `json:"kinds,omitempty"`
	Direction index.Direction `json:"direction,omitempty"`
	Budget    int             `json:"budget,omitempty"`
}

type topology struct {
	adj       *index.Adjacency
	direction index.Direction
	kinds     map[string]bool
	allowance int
	scanned   int
}

func topologyFor(g *graph.Graph, a *index.Adjacency, s Selection) (*topology, error) {
	if s.Direction == "" {
		s.Direction = index.Out
	}
	if s.Direction != index.Out && s.Direction != index.In && s.Direction != index.Both {
		return nil, graph.Fail(graph.Invalid, "direction must be out, in or both")
	}
	if s.Budget == 0 {
		s.Budget = MaxBudget
	}
	if s.Budget < 1 || s.Budget > MaxBudget || len(s.Kinds) > graph.MaxRules {
		return nil, graph.Fail(graph.Invalid, "invalid expansion budget or arc-kind count")
	}
	t := &topology{
		adj: a, direction: s.Direction, allowance: s.Budget,
		kinds: make(map[string]bool, len(s.Kinds)),
	}
	for _, kind := range s.Kinds {
		if _, ok := g.Schema.Arcs[kind]; !ok {
			return nil, graph.Fail(graph.Invalid, "unknown arc kind %q", kind)
		}
		t.kinds[kind] = true
	}
	return t, nil
}

func (t *topology) visit(from string, d index.Direction, use func(graph.Arc, string) error) error {
	return t.scan(from, d, "", use)
}

// scanFrom resumes an ID-ordered enumeration strictly after the given arc ID.
// Earlier postings are skipped by an index seek, so a continuation page never
// re-inspects rows consumed by earlier pages. It stops without an error once
// stop matched rows have been delivered.
func (t *topology) scanFrom(from string, d index.Direction, after string, stop int, use func(graph.Arc, string) error) error {
	emitted := 0
	return t.scan(from, d, after, func(a graph.Arc, next string) error {
		if err := use(a, next); err != nil {
			return err
		}
		emitted++
		if emitted == stop {
			return errWindowFull
		}
		return nil
	})
}

// errWindowFull marks a paginated scan that filled its output window. It is
// not a budget failure: later rows remain available through a continuation.
var errWindowFull = graph.Fail(graph.Budget, "adjacency window full")

func (t *topology) scan(from string, d index.Direction, after string, use func(graph.Arc, string) error) error {
	it := t.adj.Iterator(from, d, after)
	for {
		arc, next, ok := it.Next()
		if !ok {
			return nil
		}
		if t.scanned == t.allowance {
			return graph.Fail(graph.Budget, "expansion budget exhausted after %d candidate arcs", t.scanned)
		}
		t.scanned++
		if len(t.kinds) != 0 && !t.kinds[arc.Kind] {
			continue
		}
		if err := use(arc, next); err != nil {
			if err == errWindowFull {
				return nil
			}
			return err
		}
	}
}

func requireVertex(g *graph.Graph, id string) error {
	if _, ok := g.Vertices[id]; !ok {
		return graph.Fail(graph.Missing, "vertex %q is not in this revision", id)
	}
	return nil
}

func hops(n int) error {
	if n < 0 || n > MaxHops {
		return graph.Fail(graph.Invalid, "max_hops must be in [0,%d]", MaxHops)
	}
	return nil
}
