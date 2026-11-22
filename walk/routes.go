package walk

import (
	"container/heap"
	"slices"

	"rillweave/graph"
	"rillweave/index"
)

// RouteRequest asks for the cheapest route from one root to each of several
// targets under a shared selection. Routes are not hop-bounded; the expansion
// budget still caps candidate arc inspections.
type RouteRequest struct {
	From    string
	Targets []string
	Selection
}

// Route is the cheapest witnessing route to one target.
type Route struct {
	Cost     int64
	Vertices []string
	Arcs     []string
}

// RoutesResult maps each requested target to its route when one exists.
type RoutesResult struct {
	Routes  map[string]Route
	Scanned int
}

// Routes answers many reachability questions with one traversal so callers
// that share a root and selection pay for a single expansion. The cheapest
// route wins, with the same tie-breaking as Path: fewer arcs, then the
// lexicographically smallest arc ID sequence.
func Routes(g *graph.Graph, a *index.Adjacency, q RouteRequest) (RoutesResult, error) {
	t, err := topologyFor(g, a, q.Selection)
	if err != nil {
		return RoutesResult{}, err
	}
	if err := requireVertex(g, q.From); err != nil {
		return RoutesResult{}, err
	}
	pending := make(map[string]bool, len(q.Targets))
	for _, target := range q.Targets {
		if err := requireVertex(g, target); err != nil {
			return RoutesResult{}, err
		}
		pending[target] = true
	}
	start := label{vertices: []string{q.From}, arcs: []string{}}
	best := map[string]label{q.From: start}
	queue := &labelQueue{{vertex: q.From, label: start}}
	found := make(map[string]Route, len(pending))
	for queue.Len() != 0 && len(pending) != 0 {
		current := heap.Pop(queue).(vertexLabel)
		leader := best[current.vertex]
		if current.cost != leader.cost || !slices.Equal(current.arcs, leader.arcs) {
			continue // stale queue entry behind an improved label
		}
		if pending[current.vertex] {
			found[current.vertex] = Route{current.cost, current.vertices, current.arcs}
			delete(pending, current.vertex)
		}
		err := t.visit(current.vertex, t.direction, func(arc graph.Arc, next string) error {
			candidate := label{
				cost:     current.cost + arc.Cost,
				arcs:     append(slices.Clone(current.arcs), arc.ID),
				vertices: append(slices.Clone(current.vertices), next),
			}
			if prior, seen := best[next]; !seen || better(candidate, prior) {
				best[next] = candidate
				heap.Push(queue, vertexLabel{next, candidate})
			}
			return nil
		})
		if err != nil {
			return RoutesResult{}, err
		}
	}
	return RoutesResult{Routes: found, Scanned: t.scanned}, nil
}

type vertexLabel struct {
	vertex string
	label
}

// labelQueue pops the most promising label first, ordered by better. Costs
// are strictly positive, so a popped label is final for its vertex.
type labelQueue []vertexLabel

func (q labelQueue) Len() int           { return len(q) }
func (q labelQueue) Less(i, j int) bool { return better(q[i].label, q[j].label) }
func (q labelQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *labelQueue) Push(x any)        { *q = append(*q, x.(vertexLabel)) }
func (q *labelQueue) Pop() any {
	old := *q
	top := old[len(old)-1]
	*q = old[:len(old)-1]
	return top
}
