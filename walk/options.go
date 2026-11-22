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
	meter
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
		adj: a, direction: s.Direction, meter: meter{allowance: s.Budget},
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

// visit enumerates candidate arcs and bills each one before selection. The
// charge therefore covers repeated inspections and candidates discarded by
// kind; only billed candidates that pass selection reach use.
func (t *topology) visit(from string, d index.Direction, use func(graph.Arc, string) error) error {
	return t.adj.Visit(from, d, func(a graph.Arc, next string) error {
		if err := t.charge(); err != nil {
			return err
		}
		if !t.allows(a) {
			return nil
		}
		return use(a, next)
	})
}

func (t *topology) allows(a graph.Arc) bool {
	return len(t.kinds) == 0 || t.kinds[a.Kind]
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
