package walk

import (
	"slices"

	"rillweave/graph"
	"rillweave/index"
)

// ImpactRequest describes one cut analysis against a single immutable view.
// Sources, candidates and targets are treated as sets and resolved together in
// that revision, so every answer refers to the same snapshot.
//
// For each candidate the answer lists the requested targets that were
// reachable from the source set before removing the candidate and are no
// longer reachable afterwards. A vertex has that effect exactly when every
// directed route from the source set to the target passes through it. That is
// decided once from a dominator tree of the source-reachable subgraph, so the
// graph is never copied per candidate: hundreds of candidates cost one
// traversal plus one tree. Parallel arcs between the same two vertices,
// self-loops and cycles do not by themselves create an alternative route.
//
// Only forward traversal is modeled: a downstream target is reached by
// following stored arcs from a source, so a direction other than out is
// rejected instead of being silently reinterpreted. Budget counts candidate
// arc inspections exactly like the other walkers; exhaustion rejects the whole
// query and never returns a partial impact set.
type ImpactRequest struct {
	Sources    []string `json:"sources"`
	Candidates []string `json:"candidates"`
	Targets    []string `json:"targets"`
	Direction  string   `json:"direction,omitempty"`
	Kinds      []string `json:"kinds,omitempty"`
	Budget     int      `json:"budget,omitempty"`
}

// Witness is one shortest directed route from a source to a lost target. It
// shows what was reachable before the removal; it is not a second copy of the
// graph.
type Witness struct {
	Source   string   `json:"source"`
	Hops     int      `json:"hops"`
	Vertices []string `json:"vertices"`
	Arcs     []string `json:"arcs"`
}

type LostTarget struct {
	Target  string  `json:"target"`
	Hops    int     `json:"hops"`
	Witness Witness `json:"witness"`
}

type CandidateImpact struct {
	Candidate string       `json:"candidate"`
	Lost      []LostTarget `json:"lost"`
}

// Target status describes how a requested target took part in the analysis.
const (
	TargetReachable   = "reachable"   // reachable now, so a candidate may lose it
	TargetUnreachable = "unreachable" // no directed route in the selected subgraph
)

type TargetState struct {
	Target string `json:"target"`
	Status string `json:"status"`
	Hops   int    `json:"hops,omitempty"`
}

// ImpactScope states exactly what the answer covers, so a small impact is
// distinguishable from an incomplete one. A candidate that no route from a
// source reaches cannot sit on any route and is listed as outside; a reached
// candidate whose removal changes nothing is listed as unaffected. Both groups
// are omitted from impact because their per-candidate entry would be empty.
type ImpactScope struct {
	Unaffected []string `json:"unaffected_candidates"`
	Outside    []string `json:"outside_candidates"`
}

type ImpactResult struct {
	Sources   []string          `json:"sources"`
	Direction string            `json:"direction"`
	Kinds     []string          `json:"kinds,omitempty"`
	Scanned   int               `json:"scanned"`
	Budget    int               `json:"budget"`
	Targets   []TargetState     `json:"targets"`
	Impact    []CandidateImpact `json:"impact"`
	Scope     ImpactScope       `json:"scope"`
}

type edgeRef struct {
	to  string
	arc string
}

// Impact implements the single-view cut analysis described by ImpactRequest.
func Impact(g *graph.Graph, a *index.Adjacency, q ImpactRequest) (ImpactResult, error) {
	if q.Direction != "" && q.Direction != string(index.Out) {
		return ImpactResult{}, graph.Fail(graph.Invalid, "impact follows stored arcs forward; direction must be out")
	}
	t, err := topologyFor(g, a, Selection{Kinds: q.Kinds, Direction: index.Out, Budget: q.Budget})
	if err != nil {
		return ImpactResult{}, err
	}
	sources, err := resolveSet(g, q.Sources)
	if err != nil {
		return ImpactResult{}, err
	}
	targets, err := resolveSet(g, q.Targets)
	if err != nil {
		return ImpactResult{}, err
	}
	candidates, err := resolveSet(g, q.Candidates)
	if err != nil {
		return ImpactResult{}, err
	}
	if len(sources) == 0 || len(targets) == 0 || len(candidates) == 0 {
		return ImpactResult{}, graph.Fail(graph.Invalid, "sources, candidates and targets must each be nonempty")
	}

	// Sources and targets are fixed. A candidate has to be an intermediate
	// vertex removable on its own, so naming one in another role is a
	// malformed request rather than a silent no-op.
	sourceSet, targetSet := set(sources), set(targets)
	for _, id := range candidates {
		switch {
		case sourceSet[id]:
			return ImpactResult{}, graph.Fail(graph.Invalid, "candidate %q is also a source; sources and targets are not candidates", id)
		case targetSet[id]:
			return ImpactResult{}, graph.Fail(graph.Invalid, "candidate %q is also a target; sources and targets are not candidates", id)
		}
	}

	// One breadth-first exploration of the selected subgraph. Every candidate
	// arc is charged before the kind filter, like the other walkers. The
	// discovery tree records one shortest route to each vertex and supplies
	// the witnesses later.
	dist := make(map[string]int, len(sources))
	parent := make(map[string]string, len(sources))
	parentArc := make(map[string]string, len(sources))
	succ := make(map[string][]edgeRef)
	queue := make([]string, 0, len(sources))
	for _, source := range sources {
		if _, seen := dist[source]; !seen {
			dist[source] = 0
			queue = append(queue, source)
		}
	}
	for head := 0; head < len(queue); head++ {
		vertex := queue[head]
		err := t.visit(vertex, index.Out, func(arc graph.Arc, next string) error {
			succ[vertex] = append(succ[vertex], edgeRef{next, arc.ID})
			if _, seen := dist[next]; !seen {
				dist[next] = dist[vertex] + 1
				parent[next] = vertex
				parentArc[next] = arc.ID
				queue = append(queue, next)
			}
			return nil
		})
		if err != nil {
			return ImpactResult{}, graph.Fail(graph.Budget,
				"expansion budget exhausted after %d candidate arcs; %d vertices discovered and %d frontier entries unexpanded; no partial impact result is returned",
				t.scanned, len(dist), len(queue)-head-1)
		}
	}

	// Vertices are numbered by identity so the dominator tree, and therefore
	// the witness routes, do not depend on request or map order.
	ids := make([]string, 0, len(dist))
	for id := range dist {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	position := make(map[string]int, len(ids))
	for i, id := range ids {
		position[id] = i + 1
	}
	n := len(ids)
	sink := n + 1
	successors := make([][]int, n+2)
	predecessors := make([][]int, n+2)
	for _, id := range ids {
		from := position[id]
		unique := make(map[int]bool, len(succ[id]))
		for _, e := range succ[id] {
			unique[position[e.to]] = true
		}
		row := make([]int, 0, len(unique))
		for to := range unique {
			row = append(row, to)
		}
		slices.Sort(row)
		successors[from] = row
		for _, to := range row {
			predecessors[to] = append(predecessors[to], from)
		}
	}
	// A fresh sink fed by every source gives the source set one root. Removing
	// a candidate c loses target t exactly when c strictly dominates t, which
	// is the half-open interval test below.
	successors[sink] = make([]int, 0, len(sources))
	for _, source := range sources {
		successors[sink] = append(successors[sink], position[source])
	}
	slices.Sort(successors[sink])
	for _, to := range successors[sink] {
		predecessors[to] = append(predecessors[to], sink)
	}
	tree := newDominators(n, successors, predecessors, sink)
	tin, tout := tree.intervals()

	states := make([]TargetState, 0, len(targets))
	reachable := make([]string, 0, len(targets))
	for _, target := range targets {
		hops, ok := dist[target]
		state := TargetState{Target: target, Status: TargetUnreachable}
		if ok {
			state.Status, state.Hops = TargetReachable, hops
			reachable = append(reachable, target)
		}
		states = append(states, state)
	}

	impact := []CandidateImpact{}
	unaffected := []string{}
	outside := []string{}
	for _, candidate := range candidates {
		at, ok := position[candidate]
		if !ok {
			// No route from a source reaches this candidate, so it cannot
			// interrupt one. This is structural, not a budget effect.
			outside = append(outside, candidate)
			continue
		}
		lost := []LostTarget{}
		for _, target := range reachable {
			to := position[target]
			if tin[at] <= tin[to] && tin[to] < tout[at] {
				lost = append(lost, LostTarget{target, dist[target], witness(target, parent, parentArc)})
			}
		}
		if len(lost) == 0 {
			unaffected = append(unaffected, candidate)
			continue
		}
		impact = append(impact, CandidateImpact{candidate, lost})
	}
	return ImpactResult{
		Sources:   sources,
		Direction: string(index.Out),
		Kinds:     q.Kinds,
		Scanned:   t.scanned,
		Budget:    t.allowance,
		Targets:   states,
		Impact:    impact,
		Scope:     ImpactScope{Unaffected: unaffected, Outside: outside},
	}, nil
}

// resolveSet validates every requested identifier in the revision and removes
// repeats. Sets are returned sorted, so neither the analysis nor the output
// depends on request order.
func resolveSet(g *graph.Graph, ids []string) ([]string, error) {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if err := requireVertex(g, id); err != nil {
			return nil, err
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	slices.Sort(out)
	return out, nil
}

func set(ids []string) map[string]bool {
	out := make(map[string]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// witness reconstructs the shortest discovery route to target. The parent maps
// always end at a source: every recorded parent was discovered earlier.
func witness(target string, parent, parentArc map[string]string) Witness {
	vertices := []string{target}
	arcs := []string{}
	for current := target; ; {
		previous, ok := parent[current]
		if !ok {
			break
		}
		arcs = append(arcs, parentArc[current])
		vertices = append(vertices, previous)
		current = previous
	}
	slices.Reverse(vertices)
	slices.Reverse(arcs)
	return Witness{Source: vertices[0], Hops: len(arcs), Vertices: vertices, Arcs: arcs}
}
