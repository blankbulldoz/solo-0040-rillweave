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

// meter is one candidate-inspection allowance. Several topologies may share
// it so that a query with more than one staging step cannot spend the whole
// budget twice.
type meter struct {
	allowance int
	scanned   int
}

type topology struct {
	adj       *index.Adjacency
	direction index.Direction
	kinds     map[string]bool
	charge    *meter
}

func topologyFor(g *graph.Graph, a *index.Adjacency, s Selection) (*topology, error) {
	charge, err := newMeter(s.Budget)
	if err != nil {
		return nil, err
	}
	return topologyShared(g, a, s, charge)
}

func newMeter(budget int) (*meter, error) {
	if budget == 0 {
		budget = MaxBudget
	}
	if budget < 1 || budget > MaxBudget {
		return nil, graph.Fail(graph.Invalid, "invalid expansion budget")
	}
	return &meter{allowance: budget}, nil
}

func topologyShared(g *graph.Graph, a *index.Adjacency, s Selection, charge *meter) (*topology, error) {
	if s.Direction == "" {
		s.Direction = index.Out
	}
	if s.Direction != index.Out && s.Direction != index.In && s.Direction != index.Both {
		return nil, graph.Fail(graph.Invalid, "direction must be out, in or both")
	}
	if len(s.Kinds) > graph.MaxRules {
		return nil, graph.Fail(graph.Invalid, "invalid expansion budget or arc-kind count")
	}
	t := &topology{
		adj: a, direction: s.Direction, charge: charge,
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

func (t *topology) scanned() int {
	return t.charge.scanned
}

func (t *topology) visit(from string, d index.Direction, use func(graph.Arc, string) error) error {
	return t.adj.Visit(from, d, func(a graph.Arc, next string) error {
		if t.charge.scanned == t.charge.allowance {
			return graph.Fail(graph.Budget, "expansion budget exhausted after %d candidate arcs", t.charge.scanned)
		}
		t.charge.scanned++
		if len(t.kinds) != 0 && !t.kinds[a.Kind] {
			return nil
		}
		return use(a, next)
	})
}

// visitEach expands several vertices in one pass with the same charge, so a
// caller that already holds a whole frontier does not need one callback per
// vertex.
func (t *topology) visitEach(vertices []string, use func(vertex string, arc graph.Arc, next string) error) error {
	for _, vertex := range vertices {
		err := t.visit(vertex, t.direction, func(a graph.Arc, next string) error {
			return use(vertex, a, next)
		})
		if err != nil {
			return err
		}
	}
	return nil
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
