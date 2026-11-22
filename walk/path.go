package walk

import (
	"slices"

	"rillweave/graph"
	"rillweave/index"
)

type PathRequest struct {
	From              string          `json:"from"`
	To                string          `json:"to"`
	MaxHops           int             `json:"max_hops"`
	BlockedRelayKinds []string        `json:"blocked_relay_kinds,omitempty"`
	PropertyLimits    []PropertyLimit `json:"property_limits,omitempty"`
	Selection
}

// PropertyLimit bounds how many vertices of one route carry Property with the
// exact typed Value. A vertex missing the property, or carrying another value
// (including bool false when Value is true), does not spend the limit.
type PropertyLimit struct {
	Property string       `json:"property"`
	Value    graph.Scalar `json:"value"`
	Max      int          `json:"max"`
}

type PathResult struct {
	Found    bool     `json:"found"`
	Cost     int64    `json:"cost"`
	Vertices []string `json:"vertices"`
	Arcs     []string `json:"arcs"`
	Scanned  int      `json:"scanned"`
}

// One counter slot per declared property limit. The fixed size keeps the
// vector a comparable map key; unused slots stay zero.
type countVector [graph.MaxProps]int

type label struct {
	cost     int64
	used     countVector
	arcs     []string
	vertices []string
}

type routeKey struct {
	vertex string
	used   countVector
}

type limitKey struct {
	property string
	value    graph.Scalar
}

type routePolicy struct {
	blockedRelay map[string]bool
	limits       []PropertyLimit
}

func routePolicyFor(g *graph.Graph, q PathRequest) (routePolicy, error) {
	if len(q.BlockedRelayKinds) > graph.MaxRules || len(q.PropertyLimits) > graph.MaxProps {
		return routePolicy{}, graph.Fail(graph.Invalid, "too many blocked relay kinds or property limits")
	}
	policy := routePolicy{blockedRelay: make(map[string]bool, len(q.BlockedRelayKinds))}
	for _, kind := range q.BlockedRelayKinds {
		if _, ok := g.Schema.Vertices[kind]; !ok {
			return routePolicy{}, graph.Fail(graph.Invalid, "unknown vertex kind %q", kind)
		}
		policy.blockedRelay[kind] = true
	}
	seen := make(map[limitKey]bool, len(q.PropertyLimits))
	for _, limit := range q.PropertyLimits {
		if !graph.ValidID(limit.Property) || !limit.Value.Valid() {
			return routePolicy{}, graph.Fail(graph.Invalid, "property limit requires a declared property and a typed value")
		}
		if limit.Max < 0 || limit.Max > q.MaxHops+1 {
			return routePolicy{}, graph.Fail(graph.Invalid, "property visit max must be in [0,%d]", q.MaxHops+1)
		}
		declared := false
		for _, rule := range g.Schema.Vertices {
			field, ok := rule.Properties[limit.Property]
			if !ok {
				continue
			}
			declared = true
			if field.Kind != limit.Value.Kind {
				return routePolicy{}, graph.Fail(graph.Invalid, "property %q requires %s", limit.Property, field.Kind)
			}
		}
		if !declared {
			return routePolicy{}, graph.Fail(graph.Invalid, "unknown property %q", limit.Property)
		}
		key := limitKey{limit.Property, limit.Value}
		if seen[key] {
			return routePolicy{}, graph.Fail(graph.Invalid, "duplicate property limit for %q", limit.Property)
		}
		seen[key] = true
		policy.limits = append(policy.limits, limit)
	}
	return policy, nil
}

// count adds the limits matched by v. Exact presence and typed equality are
// required: an absent property never matches, and bool false is not bool true.
func (p routePolicy) count(v graph.Vertex, used *countVector) {
	for i, limit := range p.limits {
		have, ok := v.Properties[limit.Property]
		if ok && have == limit.Value {
			used[i]++
		}
	}
}

func (p routePolicy) within(used countVector) bool {
	for i, limit := range p.limits {
		if used[i] > limit.Max {
			return false
		}
	}
	return true
}

// relay reports whether entering target is forbidden. The destination is a
// legal endpoint even when its kind is blocked as a relay.
func (p routePolicy) relay(target, destination string, kind string) bool {
	return target != destination && p.blockedRelay[kind]
}

func Path(g *graph.Graph, a *index.Adjacency, q PathRequest) (PathResult, error) {
	if err := hops(q.MaxHops); err != nil {
		return PathResult{}, err
	}
	t, err := topologyFor(g, a, q.Selection)
	if err != nil {
		return PathResult{}, err
	}
	policy, err := routePolicyFor(g, q)
	if err != nil {
		return PathResult{}, err
	}
	for _, v := range []string{q.From, q.To} {
		if err := requireVertex(g, v); err != nil {
			return PathResult{}, err
		}
	}
	initial := label{vertices: []string{q.From}, arcs: []string{}}
	policy.count(g.Vertices[q.From], &initial.used)
	noRoute := PathResult{Vertices: []string{}, Arcs: []string{}, Scanned: t.scanned}
	if !policy.within(initial.used) {
		return noRoute, nil
	}
	if q.From == q.To {
		return PathResult{Found: true, Vertices: initial.vertices, Arcs: initial.arcs}, nil
	}
	// Labels are keyed by vertex and spent counts: the same vertex reached in
	// the same number of steps with different spent limits can differ in
	// whether a remaining route is still feasible, so labels never merge on
	// vertex identity alone.
	current := map[routeKey]label{{q.From, initial.used}: initial}
	var best label
	found := false
	for depth := 0; depth < q.MaxHops && len(current) != 0; depth++ {
		next := make(map[routeKey]label)
		for key, prefix := range current {
			vertex := key.vertex
			if vertex == q.To || found && prefix.cost >= best.cost {
				continue
			}
			err = t.visit(vertex, t.direction, func(arc graph.Arc, target string) error {
				if policy.relay(target, q.To, g.Vertices[target].Kind) {
					return nil
				}
				cost := prefix.cost + arc.Cost
				if found && cost > best.cost {
					return nil
				}
				used := prefix.used
				policy.count(g.Vertices[target], &used)
				if !policy.within(used) {
					return nil
				}
				candidate := label{
					cost: cost, used: used,
					arcs:     append(slices.Clone(prefix.arcs), arc.ID),
					vertices: append(slices.Clone(prefix.vertices), target),
				}
				candidateKey := routeKey{target, used}
				prior, seen := next[candidateKey]
				if !seen || better(candidate, prior) {
					next[candidateKey] = candidate
				}
				return nil
			})
			if err != nil {
				return PathResult{}, err
			}
		}
		if candidate, ok := bestLabelAt(next, q.To); ok && (!found || better(candidate, best)) {
			best, found = candidate, true
		}
		current = prune(next)
	}
	if !found {
		return noRoute, nil
	}
	return PathResult{true, best.cost, best.vertices, best.arcs, t.scanned}, nil
}

func bestLabelAt(labels map[routeKey]label, vertex string) (label, bool) {
	var best label
	found := false
	for key, value := range labels {
		if key.vertex == vertex && (!found || better(value, best)) {
			best, found = value, true
		}
	}
	return best, found
}

// prune keeps Pareto-optimal labels per vertex. A label is dropped only when
// another label reaches the same vertex no later with no greater cost and no
// larger spent count on any limit; positive arc costs preserve that ordering
// through every extension. Distinct count vectors are never merged.
func prune(labels map[routeKey]label) map[routeKey]label {
	grouped := make(map[string][]label)
	for key, value := range labels {
		grouped[key.vertex] = append(grouped[key.vertex], value)
	}
	out := make(map[routeKey]label, len(labels))
	for vertex, group := range grouped {
		for i, candidate := range group {
			dominated := false
			for j, other := range group {
				if i != j && dominates(other, candidate) {
					dominated = true
					break
				}
			}
			if !dominated {
				out[routeKey{vertex, candidate.used}] = candidate
			}
		}
	}
	return out
}

func dominates(a, b label) bool {
	if a.cost > b.cost {
		return false
	}
	for i := range a.used {
		if a.used[i] > b.used[i] {
			return false
		}
	}
	return a.cost < b.cost || a.used != b.used
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
