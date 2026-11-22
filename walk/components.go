package walk

import (
	"cmp"
	"slices"

	"rillweave/graph"
	"rillweave/index"
)

type ComponentsRequest struct {
	Kinds  []string `json:"kinds,omitempty"`
	Budget int      `json:"budget,omitempty"`
}

type Component struct {
	ID       string   `json:"id"`
	Vertices []string `json:"vertices"`
}

type Link struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type ComponentsResult struct {
	Groups  []Component `json:"groups"`
	Links   []Link      `json:"links"`
	Scanned int         `json:"scanned"`
}

// partition is a strongly connected partition over the selected relations.
// The topology stays live so callers keep charging the same scan budget.
type partition struct {
	topology   *topology
	groups     []Component
	membership map[string]string
}

func strongComponents(g *graph.Graph, a *index.Adjacency, s Selection) (*partition, error) {
	t, err := topologyFor(g, a, s)
	if err != nil {
		return nil, err
	}
	ids := graph.Keys(g.Vertices)
	order := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if err := finish(t, id, seen, &order); err != nil {
			return nil, err
		}
	}
	clear(seen)
	groups := make([]Component, 0)
	membership := make(map[string]string, len(ids))
	for i := len(order) - 1; i >= 0; i-- {
		if seen[order[i]] {
			continue
		}
		var group []string
		if err := collect(t, order[i], seen, &group); err != nil {
			return nil, err
		}
		slices.Sort(group)
		for _, member := range group {
			membership[member] = group[0]
		}
		groups = append(groups, Component{group[0], group})
	}
	slices.SortFunc(groups, func(a, b Component) int { return cmp.Compare(a.ID, b.ID) })
	return &partition{t, groups, membership}, nil
}

func Components(g *graph.Graph, a *index.Adjacency, q ComponentsRequest) (ComponentsResult, error) {
	p, err := strongComponents(g, a, Selection{Kinds: q.Kinds, Budget: q.Budget})
	if err != nil {
		return ComponentsResult{}, err
	}
	result := ComponentsResult{Groups: p.groups, Links: []Link{}}
	links := make(map[Link]bool)
	for _, id := range graph.Keys(g.Vertices) {
		err := p.topology.visit(id, index.Out, func(_ graph.Arc, next string) error {
			from, to := p.membership[id], p.membership[next]
			if from != to {
				links[Link{from, to}] = true
			}
			return nil
		})
		if err != nil {
			return ComponentsResult{}, err
		}
	}
	for link := range links {
		result.Links = append(result.Links, link)
	}
	slices.SortFunc(result.Links, func(a, b Link) int {
		return graph.LessIdentity(a.From, a.To, b.From, b.To)
	})
	result.Scanned = p.topology.scanned
	return result, nil
}

func finish(t *topology, id string, seen map[string]bool, order *[]string) error {
	if seen[id] {
		return nil
	}
	seen[id] = true
	err := t.visit(id, index.Out, func(_ graph.Arc, next string) error {
		return finish(t, next, seen, order)
	})
	if err == nil {
		*order = append(*order, id)
	}
	return err
}

func collect(t *topology, id string, seen map[string]bool, members *[]string) error {
	if seen[id] {
		return nil
	}
	seen[id] = true
	*members = append(*members, id)
	return t.visit(id, index.In, func(_ graph.Arc, next string) error {
		return collect(t, next, seen, members)
	})
}
