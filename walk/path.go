package walk

import (
	"errors"
	"slices"

	"rillweave/graph"
	"rillweave/index"
)

// MaxPathLabels bounds the labels a single path session may retain. Within one
// exact depth only the cheapest, stable prefix per vertex is kept, so each
// frontier holds at most one label per graph vertex; the search keeps two
// frontiers (current and next), hence twice the graph's vertex limit. The cap
// is checked against both frontiers together rather than one map alone.
var MaxPathLabels = 2 * graph.MaxVertices

type PathRequest struct {
	From    string `json:"from"`
	To      string `json:"to"`
	MaxHops int    `json:"max_hops"`
	Selection
}

type PathResult struct {
	Found      bool     `json:"found"`
	Cost       int64    `json:"cost"`
	Vertices   []string `json:"vertices"`
	Arcs       []string `json:"arcs"`
	Scanned    int      `json:"scanned"`
	Complete   bool     `json:"complete"`
	UpperBound *int64   `json:"upper_bound,omitempty"`
}

type label struct {
	cost     int64
	arcs     []string
	vertices []string
}

// errPaused stops a vertex scan the moment the segment allowance is spent.
// The stored cursor makes the next Advance resume after this arc.
var errPaused = errors.New("path segment allowance spent")

// PathSearch is the resumable state of one bounded path query. Its labels hold
// every still-valid prefix candidate, so an exhausted segment never discards
// earlier work: another Advance continues the same fixed-query search.
type PathSearch struct {
	g     *graph.Graph
	adj   *index.Adjacency
	from  string
	to    string
	hops  int
	dir   index.Direction
	kinds []string
	allow map[string]bool

	depth     int
	current   map[string]label
	next      map[string]label
	order     []string
	cursor    int
	resumeArc string
	best      label
	found     bool
	scanned   int
	complete  bool
}

// NewPathSearch validates the request against the fixed graph and builds the
// initial search state. Zero-hop and empty-path cases are already complete.
func NewPathSearch(g *graph.Graph, a *index.Adjacency, q PathRequest) (*PathSearch, error) {
	if err := hops(q.MaxHops); err != nil {
		return nil, err
	}
	dir := q.Direction
	if dir == "" {
		dir = index.Out
	}
	if dir != index.Out && dir != index.In && dir != index.Both {
		return nil, graph.Fail(graph.Invalid, "direction must be out, in or both")
	}
	if len(q.Kinds) > graph.MaxRules {
		return nil, graph.Fail(graph.Invalid, "invalid arc-kind count")
	}
	for _, v := range []string{q.From, q.To} {
		if err := requireVertex(g, v); err != nil {
			return nil, err
		}
	}
	kinds := canonicalKinds(q.Kinds)
	allow := make(map[string]bool, len(kinds))
	for _, kind := range kinds {
		if _, ok := g.Schema.Arcs[kind]; !ok {
			return nil, graph.Fail(graph.Invalid, "unknown arc kind %q", kind)
		}
		allow[kind] = true
	}
	s := &PathSearch{
		g: g, adj: a, from: q.From, to: q.To, hops: q.MaxHops,
		dir: dir, kinds: kinds, allow: allow,
		current: map[string]label{},
		next:    map[string]label{},
	}
	initial := label{vertices: []string{q.From}, arcs: []string{}}
	if q.From == q.To {
		s.best, s.found, s.complete = initial, true, true
		return s, nil
	}
	// depth counts the arc length of labels held in current; expanding it
	// produces labels of depth+1 in next.
	s.depth = 0
	s.current[q.From] = initial
	s.order = graph.Keys(s.current)
	return s, nil
}

// Agreement reports whether a continuation request repeats the original
// direction, arc kinds, endpoints and hop limit. A changed search is a new
// search and must not share a session.
func (s *PathSearch) Agreement(q PathRequest) bool {
	dir := q.Direction
	if dir == "" {
		dir = index.Out
	}
	return q.From == s.from && q.To == s.to && q.MaxHops == s.hops &&
		dir == s.dir && slices.Equal(canonicalKinds(q.Kinds), s.kinds)
}

// Advance performs one search segment, charging at most allowance candidate
// arc inspections (kind-rejected arcs count, exactly like a one-shot query).
// The same search object is returned whether it paused or completed.
func (s *PathSearch) Advance(allowance int) (PathResult, error) {
	if s.complete {
		return s.Result(), nil
	}
	limit, err := BudgetLimit(allowance)
	if err != nil {
		return PathResult{}, err
	}
	segment := 0
	for !s.complete {
		// A whole layer (the labels at one exact depth) has been expanded.
		if s.cursor == len(s.order) {
			if candidate, ok := s.next[s.to]; ok && (!s.found || better(candidate, s.best)) {
				s.best, s.found = candidate, true
			}
			if s.depth == s.hops-1 || len(s.next) == 0 {
				s.complete = true
				break
			}
			s.depth++
			s.current = s.next
			s.next = map[string]label{}
			s.order, s.cursor, s.resumeArc = graph.Keys(s.current), 0, ""
			// Arc costs are strictly positive: once every surviving prefix is
			// strictly more expensive than the incumbent, no cheaper route and
			// no equal-cost route with a shorter witness can remain.
			if s.found && cheapest(s.current) > s.best.cost {
				s.complete = true
				break
			}
		}
		paused := false
		for s.cursor < len(s.order) {
			vertex := s.order[s.cursor]
			prefix := s.current[vertex]
			if vertex == s.to || s.found && prefix.cost >= s.best.cost {
				s.cursor++
				s.resumeArc = ""
				continue
			}
			scanErr := s.adj.ScanAfter(vertex, s.dir, s.resumeArc, func(arc graph.Arc, target string) error {
				if segment == limit {
					paused = true
					return errPaused
				}
				segment++
				s.scanned++
				s.resumeArc = arc.ID
				if len(s.allow) != 0 && !s.allow[arc.Kind] {
					return nil
				}
				cost := prefix.cost + arc.Cost
				prior, seen := s.next[target]
				// Strictly worse labels are dropped; equal-cost labels must
				// survive so shorter/lexicographic witnesses stay reachable.
				if seen && cost > prior.cost || s.found && cost > s.best.cost {
					return nil
				}
				candidate := label{
					cost: cost, arcs: append(slices.Clone(prefix.arcs), arc.ID),
					vertices: append(slices.Clone(prefix.vertices), target),
				}
				if !seen {
					if len(s.current)+len(s.next) >= MaxPathLabels {
						return graph.Fail(graph.Budget, "path session label limit %d exceeded", MaxPathLabels)
					}
				}
				if !seen || better(candidate, prior) {
					s.next[target] = candidate
					if target == s.to && (!s.found || better(candidate, s.best)) {
						s.best, s.found = candidate, true
					}
				}
				return nil
			})
			if scanErr != nil && !errors.Is(scanErr, errPaused) {
				return PathResult{}, scanErr
			}
			if paused {
				break
			}
			s.cursor++
			s.resumeArc = ""
		}
		if paused {
			break
		}
	}
	return s.Result(), nil
}

// Result renders the current state. A paused result exposes the incumbent as an
// upper bound but Complete stays false until Advance proves the endgame.
func (s *PathSearch) Result() PathResult {
	if !s.found {
		return PathResult{
			Found: false, Vertices: []string{}, Arcs: []string{},
			Scanned: s.scanned, Complete: s.complete,
		}
	}
	return PathResult{
		Found: true, Cost: s.best.cost, Vertices: s.best.vertices, Arcs: s.best.arcs,
		Scanned: s.scanned, Complete: s.complete, UpperBound: &s.best.cost,
	}
}

func cheapest(labels map[string]label) int64 {
	first := true
	var low int64
	for _, candidate := range labels {
		if first || candidate.cost < low {
			low, first = candidate.cost, false
		}
	}
	return low
}

func canonicalKinds(kinds []string) []string {
	if len(kinds) == 0 {
		return nil
	}
	canonical := slices.Clone(kinds)
	slices.Sort(canonical)
	return slices.Compact(canonical)
}

// Path keeps the one-shot entry with the original budget semantics: exhausting
// the allowance rejects the whole query instead of returning a partial prefix.
func Path(g *graph.Graph, a *index.Adjacency, q PathRequest) (PathResult, error) {
	s, err := NewPathSearch(g, a, q)
	if err != nil {
		return PathResult{}, err
	}
	if s.complete {
		return s.Result(), nil
	}
	result, err := s.Advance(q.Budget)
	if err != nil {
		return PathResult{}, err
	}
	if !result.Complete {
		return PathResult{}, graph.Fail(graph.Budget,
			"expansion budget exhausted after %d candidate arcs", result.Scanned)
	}
	return result, nil
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
