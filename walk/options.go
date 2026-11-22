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
	allowance, err := BudgetLimit(s.Budget)
	if err != nil {
		return nil, err
	}
	if len(s.Kinds) > graph.MaxRules {
		return nil, graph.Fail(graph.Invalid, "invalid arc-kind count")
	}
	t := &topology{
		adj: a, direction: s.Direction, allowance: allowance,
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

// BudgetLimit normalizes a per-request candidate-arc allowance. Zero or
// omitted means the standard MaxBudget; each search segment supplies its own
// allowance and is validated through the same rule.
func BudgetLimit(request int) (int, error) {
	if request == 0 {
		return MaxBudget, nil
	}
	if request < 1 || request > MaxBudget {
		return 0, graph.Fail(graph.Invalid, "expansion budget must be in [1,%d]", MaxBudget)
	}
	return request, nil
}

func (t *topology) visit(from string, d index.Direction, use func(graph.Arc, string) error) error {
	return t.adj.Visit(from, d, func(a graph.Arc, next string) error {
		if t.scanned == t.allowance {
			return graph.Fail(graph.Budget, "expansion budget exhausted after %d candidate arcs", t.scanned)
		}
		t.scanned++
		if len(t.kinds) != 0 && !t.kinds[a.Kind] {
			return nil
		}
		return use(a, next)
	})
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
