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

func Components(g *graph.Graph, a *index.Adjacency, q ComponentsRequest) (ComponentsResult, error) {
	t, err := topologyFor(g, a, Selection{Kinds: q.Kinds, Budget: q.Budget})
	if err != nil {
		return ComponentsResult{}, err
	}
	ids := graph.Keys(g.Vertices)
	order := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if err := finish(t, id, seen, &order); err != nil {
			return ComponentsResult{}, err
		}
	}
	clear(seen)
	result := ComponentsResult{Groups: []Component{}, Links: []Link{}}
	membership := make(map[string]string, len(ids))
	for i := len(order) - 1; i >= 0; i-- {
		if seen[order[i]] {
			continue
		}
		var group []string
		if err := collect(t, order[i], seen, &group); err != nil {
			return ComponentsResult{}, err
		}
		slices.Sort(group)
		for _, member := range group {
			membership[member] = group[0]
		}
		result.Groups = append(result.Groups, Component{group[0], group})
	}
	slices.SortFunc(result.Groups, func(a, b Component) int { return cmp.Compare(a.ID, b.ID) })
	links := make(map[Link]bool)
	for _, id := range ids {
		err := t.visit(id, index.Out, func(_ graph.Arc, next string) error {
			from, to := membership[id], membership[next]
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
	result.Scanned = t.scanned
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
