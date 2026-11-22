package walk

import (
	"container/heap"
	"math"
	"slices"

	"rillweave/graph"
	"rillweave/index"
)

// RoutesRequest asks one graph revision for two directed routes from the same
// source to the same target. Each route has its own hop limit: the primary
// route must fit PrimaryMaxHops and the backup route must fit BackupMaxHops.
// The routes may share vertices but never an arc identity.
type RoutesRequest struct {
	From           string `json:"from"`
	To             string `json:"to"`
	PrimaryMaxHops int    `json:"primary_max_hops"`
	BackupMaxHops  int    `json:"backup_max_hops"`
	Selection
}

type Route struct {
	Cost     int64    `json:"cost"`
	Hops     int      `json:"hops"`
	Vertices []string `json:"vertices"`
	Arcs     []string `json:"arcs"`
}

type RoutesResult struct {
	Found     bool  `json:"found"`
	TotalCost int64 `json:"total_cost"`
	Primary   Route `json:"primary"`
	Backup    Route `json:"backup"`
	Scanned   int   `json:"scanned"`
}

type routePair struct {
	primary Route
	backup  Route
	total   int64
}

// Routes selects an arc-disjoint route pair jointly. Every primary route is
// paired with the cheapest arc-disjoint backup that fits the backup hop limit,
// and the pair with the least total cost is returned. A cheap primary that
// blocks the available backups is therefore not accepted on its own: another
// primary can win whenever its pair costs less. A single route is never
// reported as a pair.
//
// Primary routes are enumerated in nondecreasing cost. Once a pair is known, a
// primary prefix costing more than best total minus the cheapest possible
// backup cannot produce a better or tied pair, so enumeration stops with a
// proof of optimality. The shared candidate-arc allowance is the controllable
// search boundary: if it runs out before that proof, the query reports budget
// exhaustion instead of an unproven answer. A proven absence of a pair is
// found:false, which is distinct from budget exhaustion.
func Routes(g *graph.Graph, a *index.Adjacency, q RoutesRequest) (RoutesResult, error) {
	if err := hops(q.PrimaryMaxHops); err != nil {
		return RoutesResult{}, err
	}
	if err := hops(q.BackupMaxHops); err != nil {
		return RoutesResult{}, err
	}
	t, err := topologyFor(g, a, q.Selection)
	if err != nil {
		return RoutesResult{}, err
	}
	for _, v := range []string{q.From, q.To} {
		if err := requireVertex(g, v); err != nil {
			return RoutesResult{}, err
		}
	}
	if q.From == q.To {
		// Two zero-hop routes share no arc and respect any hop limit.
		return RoutesResult{Found: true, Primary: emptyRoute(q.From), Backup: emptyRoute(q.From)}, nil
	}

	// The cheapest route that ignores arc-disjointness lower-bounds every
	// backup. If it does not exist, no pair can exist.
	floor, possible, err := t.boundedRoute(q.From, q.To, q.BackupMaxHops, nil, math.MaxInt64)
	if err != nil {
		return RoutesResult{}, err
	}
	if !possible {
		return RoutesResult{Primary: emptyRoute(q.From), Backup: emptyRoute(q.From), Scanned: t.scanned}, nil
	}
	lowerBackup := floor.Cost

	// Primaries are simple routes held as parent chains, one heap entry per
	// prefix. Only prefixes are visited here; a prefix that reaches the target
	// becomes a primary candidate rather than being extended further.
	pending := &routeQueue{{vertex: q.From}}
	heap.Init(pending)
	var best routePair
	found := false
	for pending.Len() != 0 {
		head := heap.Pop(pending).(*primaryLabel)
		if found && head.cost > best.total-lowerBackup {
			break
		}
		if head.vertex == q.To {
			primary := t.primaryRoute(head)
			capCost := int64(math.MaxInt64)
			if found {
				// A backup costing more cannot improve or tie the best pair.
				capCost = best.total - primary.Cost
			}
			candidate, ok, err := t.pairWith(primary, q, capCost)
			if err != nil {
				return RoutesResult{}, err
			}
			if ok && (!found || betterPair(candidate, best)) {
				best, found = candidate, true
			}
			// A simple primary route cannot pass the target again, so no
			// optimal pair extends this label.
			continue
		}
		if head.depth == q.PrimaryMaxHops {
			continue
		}
		err := t.visit(head.vertex, t.direction, func(arc graph.Arc, next string) error {
			if head.contains(next) {
				return nil
			}
			heap.Push(pending, &primaryLabel{
				parent: head, arc: arc.ID, vertex: next,
				cost: head.cost + arc.Cost, depth: head.depth + 1,
			})
			return nil
		})
		if err != nil {
			return RoutesResult{}, err
		}
	}
	if !found {
		return RoutesResult{Primary: emptyRoute(q.From), Backup: emptyRoute(q.From), Scanned: t.scanned}, nil
	}
	return RoutesResult{
		Found: true, TotalCost: best.total, Primary: best.primary,
		Backup: best.backup, Scanned: t.scanned,
	}, nil
}

// pairWith finds the cheapest backup that shares no arc with primary. The
// search stops at capCost, which is the highest backup cost that could still
// improve or tie the best pair found so far.
func (t *topology) pairWith(primary Route, q RoutesRequest, capCost int64) (routePair, bool, error) {
	forbidden := make(map[string]bool, len(primary.Arcs))
	for _, id := range primary.Arcs {
		forbidden[id] = true
	}
	backup, ok, err := t.boundedRoute(q.From, q.To, q.BackupMaxHops, forbidden, capCost)
	if err != nil || !ok {
		return routePair{}, false, err
	}
	return routePair{primary, backup, primary.Cost + backup.Cost}, true, nil
}

func emptyRoute(source string) Route {
	return Route{Vertices: []string{source}, Arcs: []string{}}
}

// primaryLabel is one simple primary prefix. Preshared parents keep the
// frontier cheap: a label stores one arc and one vertex, not a copy of the
// whole prefix.
type primaryLabel struct {
	parent *primaryLabel
	arc    string
	vertex string
	cost   int64
	depth  int
}

func (l *primaryLabel) contains(vertex string) bool {
	for step := l; step != nil; step = step.parent {
		if step.vertex == vertex {
			return true
		}
	}
	return false
}

func (t *topology) primaryRoute(label *primaryLabel) Route {
	route := Route{Cost: label.cost, Hops: label.depth, Arcs: make([]string, label.depth), Vertices: make([]string, label.depth+1)}
	for step := label; step.parent != nil; step = step.parent {
		route.Arcs[step.depth-1] = step.arc
		route.Vertices[step.depth] = step.vertex
	}
	route.Vertices[0] = label.root().vertex
	return route
}

func (l *primaryLabel) root() *primaryLabel {
	for l.parent != nil {
		l = l.parent
	}
	return l
}

// routeQueue orders primary prefixes by cost, which is what makes the optimal
// pair bound sound. The remaining keys only make equal-cost popping stable.
type routeQueue []*primaryLabel

func (q routeQueue) Len() int { return len(q) }
func (q routeQueue) Less(i, j int) bool {
	a, b := q[i], q[j]
	if a.cost != b.cost {
		return a.cost < b.cost
	}
	if a.depth != b.depth {
		return a.depth < b.depth
	}
	if a.vertex != b.vertex {
		return a.vertex < b.vertex
	}
	return a.arc < b.arc
}
func (q routeQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }
func (q *routeQueue) Push(x any)   { *q = append(*q, x.(*primaryLabel)) }
func (q *routeQueue) Pop() any {
	old := *q
	last := old[len(old)-1]
	*q = old[:len(old)-1]
	return last
}

// betterPair orders pairs by total cost, then by the primary route and finally
// by the backup route. Route order is the same stable order used by Path:
// cost, fewer arcs, lexicographically smallest arc sequence.
func betterPair(a, b routePair) bool {
	if a.total != b.total {
		return a.total < b.total
	}
	if n := compareRoute(a.primary, b.primary); n != 0 {
		return n < 0
	}
	return compareRoute(a.backup, b.backup) < 0
}

func compareRoute(a, b Route) int {
	if a.Cost != b.Cost {
		if a.Cost < b.Cost {
			return -1
		}
		return 1
	}
	if len(a.Arcs) != len(b.Arcs) {
		return len(a.Arcs) - len(b.Arcs)
	}
	return slices.Compare(a.Arcs, b.Arcs)
}

// backLabel is one prefix in the exact-depth backup search.
type backLabel struct {
	parent *backLabel
	arc    string
	vertex string
	cost   int64
	depth  int
}

// boundedRoute returns the cheapest route from source to target using at most
// maxHops arcs, skipping forbidden arc identities when supplied. Equal costs
// prefer fewer arcs and then the lexicographically smallest arc sequence.
// Candidates costing more than capCost are not expanded.
func (t *topology) boundedRoute(source, target string, maxHops int, forbidden map[string]bool, capCost int64) (Route, bool, error) {
	prev := map[string]*backLabel{source: {vertex: source}}
	var best *backLabel
	var targetSeen bool
	var bestCost int64
	for depth := 1; depth <= maxHops && len(prev) != 0; depth++ {
		next := make(map[string]*backLabel)
		for _, vertex := range graph.Keys(prev) {
			prefix := prev[vertex]
			if targetSeen && prefix.cost >= bestCost {
				continue
			}
			err := t.inspect(vertex, t.direction, forbidden, func(arc graph.Arc, reach string) error {
				cost := prefix.cost + arc.Cost
				if cost > capCost || targetSeen && cost >= bestCost {
					// Positive costs cannot shrink a prefix, and an equal-cost
					// route with more arcs is not preferred.
					return nil
				}
				candidate := &backLabel{parent: prefix, arc: arc.ID, vertex: reach, cost: cost, depth: depth}
				prior, seen := next[reach]
				if !seen || cost < prior.cost || cost == prior.cost && lessSequence(candidate, prior) {
					next[reach] = candidate
				}
				return nil
			})
			if err != nil {
				return Route{}, false, err
			}
		}
		if state, ok := next[target]; ok && (!targetSeen || state.cost < bestCost) {
			best, bestCost, targetSeen = state, state.cost, true
		}
		prev = next
	}
	if best == nil {
		return Route{}, false, nil
	}
	return t.routeFrom(best), true, nil
}

// lessSequence compares two equal-depth arc sequences lexicographically without
// allocating: both labels are walked back to the frontier root.
func lessSequence(a, b *backLabel) bool {
	var left, right [MaxHops + 1]string
	collectArcs(a, left[:a.depth])
	collectArcs(b, right[:b.depth])
	for i := 0; i < a.depth; i++ {
		if left[i] != right[i] {
			return left[i] < right[i]
		}
	}
	return false
}

// collectArcs writes the chain's arc IDs into out in traversal order. The
// chain holds exactly len(out) arcs.
func collectArcs(label *backLabel, out []string) {
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = label.arc
		label = label.parent
	}
}

func (t *topology) routeFrom(label *backLabel) Route {
	route := Route{Cost: label.cost, Hops: label.depth, Arcs: make([]string, label.depth), Vertices: make([]string, label.depth+1)}
	for step := label; step.parent != nil; step = step.parent {
		route.Arcs[step.depth-1] = step.arc
		route.Vertices[step.depth] = step.vertex
	}
	route.Vertices[0] = label.rootVertex()
	return route
}

func (l *backLabel) rootVertex() string {
	for l.parent != nil {
		l = l.parent
	}
	return l.vertex
}
