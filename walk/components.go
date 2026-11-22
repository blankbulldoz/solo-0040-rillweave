package walk

import (
	"cmp"
	"slices"

	"rillweave/graph"
	"rillweave/index"
)

type ComponentsRequest struct {
	Kinds []string `json:"kinds,omitempty"`
	// Expand, when present, opens one confirmed directed group link and
	// returns the original arcs behind it. The strongly connected partition
	// is always computed first and never recomputed for an expansion.
	Expand      *LinkExpand `json:"expand,omitempty"`
	ExpectedRev uint64      `json:"expected_revision,omitempty"`
	Budget      int         `json:"budget,omitempty"`
}

type LinkExpand struct {
	From string `json:"from"`
	To   string `json:"to"`
	// Kind narrows the expansion to one arc kind. It must remain inside the
	// request's kind selection; browsing never widens the grouping scope.
	Kind  string `json:"kind,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

type Component struct {
	ID       string   `json:"id"`
	Vertices []string `json:"vertices"`
}

type Link struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// LinkKindCount is the exact number of original arcs of one kind behind a
// wide group link. Counts are complete even when the returned arc rows are
// truncated by the limit.
type LinkKindCount struct {
	Kind    string `json:"kind"`
	Matched int    `json:"matched"`
}

// LinkKindBucket carries the original arcs of one kind behind a group link.
// Parallel arcs keep their individual identities.
type LinkKindBucket struct {
	Kind    string      `json:"kind"`
	Arcs    []graph.Arc `json:"arcs"`
	Matched int         `json:"matched"`
}

type LinkDetail struct {
	From      string           `json:"from"`
	To        string           `json:"to"`
	Revision  uint64           `json:"revision"`
	Kinds     []LinkKindCount  `json:"counts"`
	Buckets   []LinkKindBucket `json:"buckets"`
	Truncated bool             `json:"truncated"`
	Scanned   int              `json:"scanned"`
}

type ComponentsResult struct {
	Groups   []Component `json:"groups"`
	Links    []Link      `json:"links"`
	Expanded *LinkDetail `json:"expanded,omitempty"`
	Scanned  int         `json:"scanned"`
}

func Components(g *graph.Graph, a *index.Adjacency, q ComponentsRequest) (ComponentsResult, error) {
	// Opening an old page after the graph advanced must surface the revision
	// of the confirmed partition rather than silently regrouping new members.
	if q.ExpectedRev != 0 && q.ExpectedRev != g.Revision {
		return ComponentsResult{}, graph.Fail(graph.Conflict,
			"component partition was confirmed at revision %d but the selected view is now revision %d",
			q.ExpectedRev, g.Revision)
	}
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
	if q.Expand != nil {
		detail, err := expandLink(g, t, result.Groups, membership, links, g.Revision, *q.Expand)
		if err != nil {
			return ComponentsResult{}, err
		}
		result.Expanded = detail
	}
	result.Scanned = t.scanned
	return result, nil
}

func expandLink(
	g *graph.Graph,
	t *topology,
	groups []Component,
	membership map[string]string,
	links map[Link]bool,
	revision uint64,
	req LinkExpand,
) (*LinkDetail, error) {
	if req.From == "" || req.To == "" {
		return nil, graph.Fail(graph.Invalid, "expand requires both link endpoint group ids")
	}
	if req.From == req.To {
		return nil, graph.Fail(graph.Invalid, "expand target must be a link between two distinct groups")
	}
	byID := make(map[string]Component, len(groups))
	for _, group := range groups {
		byID[group.ID] = group
	}
	source, ok := byID[req.From]
	if !ok {
		return nil, graph.Fail(graph.Missing, "group %q is not part of this partition", req.From)
	}
	if _, ok := byID[req.To]; !ok {
		return nil, graph.Fail(graph.Missing, "group %q is not part of this partition", req.To)
	}
	if !links[Link{req.From, req.To}] {
		return nil, graph.Fail(graph.Missing, "no directed link from group %q to group %q in this partition", req.From, req.To)
	}
	if req.Kind != "" {
		if _, ok := g.Schema.Arcs[req.Kind]; !ok {
			return nil, graph.Fail(graph.Invalid, "unknown arc kind %q", req.Kind)
		}
		if len(t.kinds) != 0 && !t.kinds[req.Kind] {
			return nil, graph.Fail(graph.Invalid, "arc kind %q is outside the component kind selection", req.Kind)
		}
	}
	limit, err := Limit(req.Limit)
	if err != nil {
		return nil, err
	}
	// Browsing one kind still reports the precise counts of every crossing
	// kind, so a concurrent multi-kind wide link stays auditable.
	matchedByKind := make(map[string]int)
	arcsByKind := make(map[string][]graph.Arc)
	for _, member := range source.Vertices {
		err := t.visit(member, index.Out, func(arc graph.Arc, next string) error {
			if membership[next] != req.To {
				return nil
			}
			matchedByKind[arc.Kind]++
			if (req.Kind == "" || arc.Kind == req.Kind) && len(arcsByKind[arc.Kind]) < limit {
				arcsByKind[arc.Kind] = append(arcsByKind[arc.Kind], arc)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	kindNames := make([]string, 0, len(matchedByKind))
	for kind := range matchedByKind {
		kindNames = append(kindNames, kind)
	}
	slices.Sort(kindNames)
	detail := &LinkDetail{
		From: req.From, To: req.To, Revision: revision,
		Kinds:   make([]LinkKindCount, 0, len(kindNames)),
		Buckets: []LinkKindBucket{},
	}
	for _, kind := range kindNames {
		detail.Kinds = append(detail.Kinds, LinkKindCount{Kind: kind, Matched: matchedByKind[kind]})
		if req.Kind != "" && kind != req.Kind {
			continue
		}
		arcs := arcsByKind[kind]
		// Out-adjacency visits by arc id per vertex; re-sort across the whole
		// source group for a stable, deterministic review order.
		slices.SortFunc(arcs, func(a, b graph.Arc) int { return cmp.Compare(a.ID, b.ID) })
		truncated := matchedByKind[kind] > len(arcs)
		detail.Truncated = detail.Truncated || truncated
		detail.Buckets = append(detail.Buckets, LinkKindBucket{
			Kind: kind, Arcs: arcs, Matched: matchedByKind[kind],
		})
	}
	if req.Kind != "" && len(detail.Buckets) == 0 {
		detail.Buckets = []LinkKindBucket{{Kind: req.Kind, Arcs: []graph.Arc{}, Matched: 0}}
	}
	detail.Scanned = t.scanned
	return detail, nil
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
