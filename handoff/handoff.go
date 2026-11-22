// Package handoff derives a bounded, independently installable region graph
// from one fixed view of another graph. The caller supplies starting
// vertices, allowed arc kinds and a hop limit. Region vertices are the real
// vertices reachable forward inside those bounds; every original allowed-kind
// arc whose endpoints both belong to the region is delivered unchanged.
//
// Allowed-kind arcs that point from the region at a vertex outside it are
// boundary crossings. In references mode the outside endpoint is kept only as
// an identity reference with no kind or properties. In closure mode the real
// endpoint is absorbed into the region and expansion continues under the same
// rules until no crossing remains; absorption can therefore reach beyond the
// original hop limit.
package handoff

import (
	"cmp"
	"slices"

	"rillweave/graph"
	"rillweave/index"
)

const (
	ModeReferences = "references"
	ModeClosure    = "closure"

	// MaxBudget mirrors the walk package: candidate arc inspections, including
	// arcs rejected by the kind selection, are bounded by the same allowance.
	MaxBudget = 300000
)

type Request struct {
	// Target is the name under which the derived graph is installed.
	Target  string   `json:"target"`
	Roots   []string `json:"roots"`
	Kinds   []string `json:"kinds,omitempty"`
	MaxHops int      `json:"max_hops"`
	// Mode is "references" (default) or "closure".
	Mode string `json:"mode,omitempty"`
	// Budget zero means MaxBudget.
	Budget int `json:"budget,omitempty"`
}

type Result struct {
	// Graph is the installable derived graph. Its name is not set; the engine
	// assigns the target identity at publication time, as image restore does.
	Graph    *graph.Graph
	Manifest graph.Handoff
}

type scanner struct {
	adj       *index.Adjacency
	kinds     map[string]bool
	allowance int
	scanned   int
}

func newScanner(g *graph.Graph, adj *index.Adjacency, kinds []string, budget int) (*scanner, error) {
	if budget == 0 {
		budget = MaxBudget
	}
	if budget < 1 || budget > MaxBudget {
		return nil, graph.Fail(graph.Invalid, "budget must be in [1,%d]", MaxBudget)
	}
	s := &scanner{
		adj: adj, allowance: budget,
		kinds: make(map[string]bool, len(kinds)),
	}
	for _, kind := range kinds {
		if _, ok := g.Schema.Arcs[kind]; !ok {
			return nil, graph.Fail(graph.Invalid, "unknown arc kind %q", kind)
		}
		s.kinds[kind] = true
	}
	return s, nil
}

// out enumerates the outgoing arcs of a vertex in arc ID order. Every
// candidate, including one rejected by the kind selection, consumes budget.
func (s *scanner) out(vertex string, use func(graph.Arc) error) error {
	return s.adj.Visit(vertex, index.Out, func(arc graph.Arc, _ string) error {
		if s.scanned == s.allowance {
			return graph.Fail(graph.Budget, "handoff expansion budget exhausted after %d candidate arcs", s.scanned)
		}
		s.scanned++
		if len(s.kinds) != 0 && !s.kinds[arc.Kind] {
			return nil
		}
		return use(arc)
	})
}

// Build derives the handoff region from the given immutable view. It never
// mutates the source graph and performs no installation itself.
func Build(source *graph.Graph, adj *index.Adjacency, q Request) (Result, error) {
	if q.MaxHops < 0 || q.MaxHops > 32 {
		return Result{}, graph.Fail(graph.Invalid, "max_hops must be in [0,32]")
	}
	mode := q.Mode
	if mode == "" {
		mode = ModeReferences
	}
	if mode != ModeReferences && mode != ModeClosure {
		return Result{}, graph.Fail(graph.Invalid, "mode must be %q or %q", ModeReferences, ModeClosure)
	}
	if len(q.Roots) == 0 {
		return Result{}, graph.Fail(graph.Invalid, "handoff requires at least one root")
	}
	if len(q.Roots) > graph.MaxVertices {
		return Result{}, graph.Fail(graph.Invalid, "too many roots")
	}
	seenRoots := make(map[string]bool, len(q.Roots))
	for _, root := range q.Roots {
		if !graph.ValidID(root) {
			return Result{}, graph.Fail(graph.Invalid, "invalid root %q", root)
		}
		if seenRoots[root] {
			return Result{}, graph.Fail(graph.Invalid, "duplicate root %q", root)
		}
		seenRoots[root] = true
		if _, ok := source.Vertices[root]; !ok {
			return Result{}, graph.Fail(graph.Missing, "vertex %q is not in this revision", root)
		}
	}
	s, err := newScanner(source, adj, q.Kinds, q.Budget)
	if err != nil {
		return Result{}, err
	}

	region := make(map[string]bool)
	depth := make(map[string]int)
	queue := append([]string{}, q.Roots...)
	for _, root := range queue {
		region[root] = true
		depth[root] = 0
	}
	// Arcs whose endpoints are both inside the region, keyed by original arc
	// ID so parallel arcs and loops keep their individual identities.
	internal := make(map[string]graph.Arc)
	// Outside endpoint -> original crossing arcs, every identity retained.
	crossing := make(map[string][]graph.Arc)

	// absorb moves a real vertex into the region and schedules its expansion,
	// then records the original arc. Already included vertices are not requeued.
	absorb := func(target string, d int, arc graph.Arc) {
		if !region[target] {
			region[target] = true
			depth[target] = d
			queue = append(queue, target)
		}
		internal[arc.ID] = arc
	}

	for len(queue) != 0 {
		vertex := queue[0]
		queue = queue[1:]
		// In references mode the hop limit is a hard boundary: an allowed arc
		// leaving a vertex at that depth cannot absorb its endpoint and becomes
		// a boundary reference instead. In closure mode absorbed endpoints are
		// expanded without the original limit, so the frontier never stops.
		atHopLimit := mode == ModeReferences && depth[vertex] == q.MaxHops
		err := s.out(vertex, func(arc graph.Arc) error {
			target := arc.To
			switch {
			case region[target]:
				// Reaching an already included vertex (possibly first seen on a
				// longer path) still delivers the original arc.
				internal[arc.ID] = arc
			case atHopLimit:
				crossing[target] = append(crossing[target], arc)
			default:
				// References mode absorbs within the hop limit; closure keeps
				// absorbing beyond it until no crossing arc remains.
				absorb(target, depth[vertex]+1, arc)
			}
			return nil
		})
		if err != nil {
			return Result{}, err
		}
	}

	// Closure must leave no crossing: every outside endpoint was absorbed and
	// expanded via the queue. Guard the invariant rather than trusting flow.
	if mode == ModeClosure && len(crossing) != 0 {
		return Result{}, graph.Fail(graph.Unavailable, "closure retained boundary references")
	}

	if err := capacity(region, internal); err != nil {
		return Result{}, err
	}

	references := make([]graph.Reference, 0, len(crossing))
	for _, id := range graph.Keys(crossing) {
		arcs := slices.Clone(crossing[id])
		slices.SortFunc(arcs, func(a, b graph.Arc) int { return cmp.Compare(a.ID, b.ID) })
		references = append(references, graph.Reference{Vertex: id, IncomingArcs: arcs})
	}
	regionIDs := graph.Keys(region)

	derived := graph.New("", source.Schema)
	derived.Revision = 1
	derived.Vertices = make(map[string]graph.Vertex, len(region))
	for _, id := range regionIDs {
		derived.Vertices[id] = source.Vertices[id]
	}
	derived.Arcs = make(map[string]graph.Arc, len(internal))
	for _, id := range graph.Keys(internal) {
		derived.Arcs[id] = internal[id]
	}
	kinds := slices.Clone(q.Kinds)
	slices.Sort(kinds)
	derived.Handoff = &graph.Handoff{
		Source:     source.Name,
		Revision:   source.Revision,
		Mode:       mode,
		Roots:      slices.Clone(q.Roots),
		Kinds:      kinds,
		MaxHops:    q.MaxHops,
		Scanned:    s.scanned,
		Region:     regionIDs,
		References: references,
	}
	return Result{Graph: derived, Manifest: *derived.Handoff}, nil
}

func capacity(region map[string]bool, internal map[string]graph.Arc) error {
	if len(region) > graph.MaxVertices {
		return graph.Fail(graph.Budget, "handoff region has %d vertices; installable limit is %d", len(region), graph.MaxVertices)
	}
	if len(internal) > graph.MaxArcs {
		return graph.Fail(graph.Budget, "handoff region has %d arcs; installable limit is %d", len(internal), graph.MaxArcs)
	}
	return nil
}
