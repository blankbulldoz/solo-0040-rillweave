package walk

import (
	"cmp"
	"errors"
	"slices"

	"rillweave/graph"
	"rillweave/index"
)

// Focus modes for Components. ModeInduced groups only the subgraph induced by
// the requested vertices. ModeRelay groups the whole graph, so unlisted
// vertices may relay connectivity, but displays only requested members.
const (
	ModeInduced = "induced"
	ModeRelay   = "relay"
)

type ComponentsRequest struct {
	Kinds    []string `json:"kinds,omitempty"`
	Budget   int      `json:"budget,omitempty"`
	Vertices []string `json:"vertices,omitempty"`
	Mode     string   `json:"mode,omitempty"`
}

type Component struct {
	ID       string   `json:"id"`
	Vertices []string `json:"vertices"`
}

// Link is a directed quotient edge between two displayed groups. Arcs lists
// the IDs of the original arcs between displayed members that witness it.
type Link struct {
	From string   `json:"from"`
	To   string   `json:"to"`
	Arcs []string `json:"arcs"`
}

type ComponentsResult struct {
	Groups  []Component `json:"groups"`
	Links   []Link      `json:"links"`
	Scanned int         `json:"scanned"`
}

func Components(g *graph.Graph, a *index.Adjacency, q ComponentsRequest) (ComponentsResult, error) {
	focus, focusIDs, err := focusSet(g, q)
	if err != nil {
		return ComponentsResult{}, err
	}
	t, err := topologyFor(g, a, Selection{Kinds: q.Kinds, Budget: q.Budget})
	if err != nil {
		return ComponentsResult{}, err
	}
	ids := graph.Keys(g.Vertices)
	var admit func(string) bool
	if focus != nil && q.Mode == ModeInduced {
		ids = focusIDs
		admit = func(id string) bool { return focus[id] }
	}
	order := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if err := finish(t, admit, id, seen, &order); err != nil {
			return ComponentsResult{}, incomplete("component grouping", err)
		}
	}
	clear(seen)
	var sccs [][]string
	for i := len(order) - 1; i >= 0; i-- {
		if seen[order[i]] {
			continue
		}
		var group []string
		if err := collect(t, admit, order[i], seen, &group); err != nil {
			return ComponentsResult{}, incomplete("component grouping", err)
		}
		slices.Sort(group)
		sccs = append(sccs, group)
	}
	result := ComponentsResult{Groups: []Component{}, Links: []Link{}}
	membership := make(map[string]string, len(ids))
	for _, scc := range sccs {
		shown := scc
		if q.Mode == ModeRelay {
			shown = make([]string, 0, len(scc))
			for _, member := range scc {
				if focus[member] {
					shown = append(shown, member)
				}
			}
		}
		if len(shown) == 0 {
			continue
		}
		for _, member := range shown {
			membership[member] = shown[0]
		}
		result.Groups = append(result.Groups, Component{shown[0], shown})
	}
	slices.SortFunc(result.Groups, func(a, b Component) int { return cmp.Compare(a.ID, b.ID) })
	if focus != nil {
		ids = focusIDs
	}
	evidence := make(map[[2]string][]string)
	for _, id := range ids {
		err := t.visit(id, index.Out, func(arc graph.Arc, next string) error {
			if focus != nil && !focus[next] {
				return nil
			}
			from, to := membership[id], membership[next]
			if from != to {
				key := [2]string{from, to}
				evidence[key] = append(evidence[key], arc.ID)
			}
			return nil
		})
		if err != nil {
			return ComponentsResult{}, incomplete("link evidence", err)
		}
	}
	for link, arcs := range evidence {
		slices.Sort(arcs)
		result.Links = append(result.Links, Link{link[0], link[1], arcs})
	}
	slices.SortFunc(result.Links, func(a, b Link) int {
		return graph.LessIdentity(a.From, a.To, b.From, b.To)
	})
	result.Scanned = t.scanned
	return result, nil
}

// focusSet validates the optional focus selection and returns the
// deduplicated member set with its sorted IDs. A nil set groups and displays
// the whole graph. Vertices without a mode, or a mode without vertices, is
// rejected so the review semantics are always an explicit choice.
func focusSet(g *graph.Graph, q ComponentsRequest) (map[string]bool, []string, error) {
	if len(q.Vertices) == 0 {
		if q.Mode != "" {
			return nil, nil, graph.Fail(graph.Invalid, "mode %q requires a nonempty vertices list", q.Mode)
		}
		return nil, nil, nil
	}
	if q.Mode != ModeInduced && q.Mode != ModeRelay {
		return nil, nil, graph.Fail(graph.Invalid, "mode must be %q or %q when vertices are listed", ModeInduced, ModeRelay)
	}
	if len(q.Vertices) > graph.MaxVertices {
		return nil, nil, graph.Fail(graph.Invalid, "vertices lists at most %d vertices", graph.MaxVertices)
	}
	focus := make(map[string]bool, len(q.Vertices))
	for _, id := range q.Vertices {
		if err := requireVertex(g, id); err != nil {
			return nil, nil, err
		}
		focus[id] = true
	}
	return focus, graph.Keys(focus), nil
}

// incomplete reports which determination a budget failure interrupted, so a
// review can tell an unfinished grouping from an unfinished link scan.
func incomplete(phase string, err error) error {
	var p *graph.Problem
	if errors.As(err, &p) && p.Code == graph.Budget {
		return graph.Fail(graph.Budget, "%s did not complete: %s", phase, p.Details)
	}
	return err
}

func finish(t *topology, admit func(string) bool, id string, seen map[string]bool, order *[]string) error {
	if seen[id] {
		return nil
	}
	seen[id] = true
	err := t.visit(id, index.Out, func(_ graph.Arc, next string) error {
		if admit != nil && !admit(next) {
			return nil
		}
		return finish(t, admit, next, seen, order)
	})
	if err == nil {
		*order = append(*order, id)
	}
	return err
}

func collect(t *topology, admit func(string) bool, id string, seen map[string]bool, members *[]string) error {
	if seen[id] {
		return nil
	}
	seen[id] = true
	*members = append(*members, id)
	return t.visit(id, index.In, func(_ graph.Arc, next string) error {
		if admit != nil && !admit(next) {
			return nil
		}
		return collect(t, admit, next, seen, members)
	})
}
