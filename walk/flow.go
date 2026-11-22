package walk

import (
	"math"
	"slices"

	"rillweave/graph"
	"rillweave/index"
)

// FlowRequest estimates independent cross-group throughput between two groups
// of the strongly connected partition. The query uses only the selected arc
// kinds: vertices inside the same group are treated as freely switchable at no
// cost, while every original arc crossing a group boundary keeps its own
// positive integer capacity.
type FlowRequest struct {
	From   string   `json:"from"`
	To     string   `json:"to"`
	Limit  int64    `json:"limit,omitempty"`
	Kinds  []string `json:"kinds,omitempty"`
	Budget int      `json:"budget,omitempty"`
}

// ArcFlow is an original crossing arc and the throughput assigned to it.
type ArcFlow struct {
	Arc      string `json:"arc"`
	From     string `json:"from"`
	To       string `json:"to"`
	Capacity int64  `json:"capacity"`
	Flow     int64  `json:"flow"`
}

// CutArc is a bottleneck original arc on a minimum directed cut. Bottlenecks
// are certified only when the exact available amount was computed.
type CutArc struct {
	Arc      string `json:"arc"`
	From     string `json:"from"`
	To       string `json:"to"`
	Capacity int64  `json:"capacity"`
}

// FlowRoute is one bundle of the flow decomposition; every unit of its flow
// follows the listed arcs, so the routes certify how throughput is routed.
type FlowRoute struct {
	Groups []string `json:"groups"`
	Arcs   []string `json:"arcs"`
	Flow   int64    `json:"flow"`
}

const (
	// FlowExact means the true maximum is at or below the requested limit.
	FlowExact = "exact"
	// FlowSufficient means the requested limit was proven routable without
	// computing the full maximum.
	FlowSufficient = "sufficient"
)

type FlowResult struct {
	Found       bool        `json:"found"`
	Status      string      `json:"status"`
	Available   int64       `json:"available"`
	Limit       int64       `json:"limit"`
	Entry       string      `json:"entry"`
	Exit        string      `json:"exit"`
	Groups      []string    `json:"groups"`
	Arcs        []ArcFlow   `json:"arcs"`
	Routes      []FlowRoute `json:"routes"`
	Bottlenecks []CutArc    `json:"bottlenecks"`
	Scanned     int         `json:"scanned"`
}

// crossing records one original arc whose endpoints belong to different groups.
type crossing struct {
	arc      graph.Arc
	from     string
	to       string
	nodeFrom int
	nodeTo   int
	capacity int64
}

// Flow computes a bounded maximum flow on the group quotient. Capacities are
// pushed as whole integers rather than expanded per unit, so an arc with a very
// large capacity costs the same number of augmenting steps as any other arc.
func Flow(g *graph.Graph, a *index.Adjacency, q FlowRequest) (FlowResult, error) {
	if q.Limit < 0 {
		return FlowResult{}, graph.Fail(graph.Invalid, "limit must be a non-negative integer")
	}
	if err := requireVertex(g, q.From); err != nil {
		return FlowResult{}, err
	}
	if err := requireVertex(g, q.To); err != nil {
		return FlowResult{}, err
	}
	p, err := strongComponents(g, a, Selection{Kinds: q.Kinds, Budget: q.Budget})
	if err != nil {
		return FlowResult{}, err
	}
	entry := p.membership[q.From]
	exit := p.membership[q.To]
	if entry == exit {
		return FlowResult{}, graph.Fail(graph.Invalid, "from and to belong to the same group %q", entry)
	}
	// Collect crossing arcs through the same topology so kind selection and the
	// scan budget are charged exactly like every other structural query.
	var arcs []crossing
	for _, id := range graph.Keys(g.Vertices) {
		err := p.topology.visit(id, index.Out, func(arc graph.Arc, next string) error {
			from, to := p.membership[id], p.membership[next]
			if from != to {
				arcs = append(arcs, crossing{arc: arc, from: from, to: to, capacity: arc.Cost})
			}
			return nil
		})
		if err != nil {
			return FlowResult{}, err
		}
	}
	slices.SortFunc(arcs, func(x, y crossing) int {
		return graph.LessIdentity(x.arc.Kind, x.arc.ID, y.arc.Kind, y.arc.ID)
	})

	groups := make([]string, 0, len(p.groups))
	indexOf := make(map[string]int, len(p.groups))
	for _, group := range p.groups {
		indexOf[group.ID] = len(groups)
		groups = append(groups, group.ID)
	}
	source, sink := indexOf[entry], indexOf[exit]
	net := newFlowNetwork(len(groups))
	for i := range arcs {
		arcs[i].nodeFrom = indexOf[arcs[i].from]
		arcs[i].nodeTo = indexOf[arcs[i].to]
		net.addEdge(arcs[i].nodeFrom, arcs[i].nodeTo, arcs[i].capacity)
	}
	total := net.maxFlow(source, sink, q.Limit)
	status := FlowExact
	if q.Limit > 0 && total >= q.Limit {
		status = FlowSufficient
	}

	result := FlowResult{
		Found:       total > 0,
		Status:      status,
		Available:   total,
		Limit:       q.Limit,
		Entry:       entry,
		Exit:        exit,
		Groups:      groups,
		Arcs:        []ArcFlow{},
		Routes:      []FlowRoute{},
		Bottlenecks: []CutArc{},
		Scanned:     p.topology.scanned,
	}
	if total == 0 {
		return result, nil
	}
	// A reachable set in the final residual network certifies a minimum cut;
	// only an exact run has actually reached the true maximum.
	reachable := net.reachable(source)
	for i, edge := range arcs {
		forward := net.edges[net.refs[i]]
		if forward.flow > 0 {
			result.Arcs = append(result.Arcs, ArcFlow{
				Arc: edge.arc.ID, From: edge.from, To: edge.to,
				Capacity: edge.capacity, Flow: forward.flow,
			})
		}
		if status == FlowExact && reachable[edge.nodeFrom] && !reachable[edge.nodeTo] {
			result.Bottlenecks = append(result.Bottlenecks, CutArc{
				Arc: edge.arc.ID, From: edge.from, To: edge.to, Capacity: edge.capacity,
			})
		}
	}
	result.Routes = decompose(groups, source, sink, arcs, net)
	return result, nil
}

// decompose splits the realized flow into deterministic group-to-group routes.
// The quotient is acyclic, so positive crossing flow can always be drained to
// the exit without revisiting a group. Crossing arcs stay in arc-ID order, so
// the smallest-ID usable edge is always chosen first.
func decompose(groups []string, source, sink int, arcs []crossing, net *flowNetwork) []FlowRoute {
	remaining := make([]int64, len(arcs))
	neighbors := make(map[int][]int, len(groups))
	for i, edge := range arcs {
		remaining[i] = net.edges[net.refs[i]].flow
		if remaining[i] > 0 {
			neighbors[edge.nodeFrom] = append(neighbors[edge.nodeFrom], i)
		}
	}
	routes := []FlowRoute{}
	for {
		first := -1
		for _, ref := range neighbors[source] {
			if remaining[ref] > 0 {
				first = ref
				break
			}
		}
		if first < 0 {
			return routes
		}
		groupPath := []int{source}
		arcPath := []int{}
		for groupPath[len(groupPath)-1] != sink {
			node := groupPath[len(groupPath)-1]
			choice := -1
			for _, ref := range neighbors[node] {
				if remaining[ref] > 0 {
					choice = ref
					break
				}
			}
			if choice < 0 {
				break
			}
			arcPath = append(arcPath, choice)
			groupPath = append(groupPath, arcs[choice].nodeTo)
		}
		bundle := remaining[arcPath[0]]
		for _, ref := range arcPath[1:] {
			bundle = min(bundle, remaining[ref])
		}
		for _, ref := range arcPath {
			remaining[ref] -= bundle
		}
		route := FlowRoute{Groups: make([]string, len(groupPath)), Arcs: make([]string, len(arcPath)), Flow: bundle}
		for i, node := range groupPath {
			route.Groups[i] = groups[node]
		}
		for i, ref := range arcPath {
			route.Arcs[i] = arcs[ref].arc.ID
		}
		routes = append(routes, route)
	}
}

// flowEdge is one directed residual edge; rev points at the reverse edge.
type flowEdge struct {
	to       int
	capacity int64
	flow     int64
	rev      int
}

type flowNetwork struct {
	edges []flowEdge
	head  [][]int
	// refs maps the i-th crossing arc to its forward edge index.
	refs []int
}

func newFlowNetwork(nodes int) *flowNetwork {
	return &flowNetwork{head: make([][]int, nodes)}
}

func (n *flowNetwork) addEdge(from, to int, capacity int64) {
	ref := len(n.edges)
	n.edges = append(n.edges,
		flowEdge{to: to, capacity: capacity, rev: ref + 1},
		flowEdge{to: from, capacity: 0, rev: ref},
	)
	n.head[from] = append(n.head[from], ref)
	n.head[to] = append(n.head[to], ref+1)
	n.refs = append(n.refs, ref)
}

// maxFlow runs Dinic with bulk integer augmentations and stops once cap is
// reached. A zero cap means the full maximum is required.
func (n *flowNetwork) maxFlow(source, sink int, cap int64) int64 {
	var total int64
	for cap == 0 || total < cap {
		level := make([]int, len(n.head))
		for i := range level {
			level[i] = -1
		}
		level[source] = 0
		queue := []int{source}
		for len(queue) > 0 {
			node := queue[0]
			queue = queue[1:]
			for _, ref := range n.head[node] {
				edge := n.edges[ref]
				if edge.capacity-edge.flow > 0 && level[edge.to] < 0 {
					level[edge.to] = level[node] + 1
					queue = append(queue, edge.to)
				}
			}
		}
		if level[sink] < 0 {
			return total
		}
		cursor := make([]int, len(n.head))
		var push func(int, int64) int64
		push = func(node int, want int64) int64 {
			if node == sink || want == 0 {
				return want
			}
			for ; cursor[node] < len(n.head[node]); cursor[node]++ {
				ref := n.head[node][cursor[node]]
				edge := n.edges[ref]
				if edge.capacity-edge.flow == 0 || level[edge.to] != level[node]+1 {
					continue
				}
				sent := push(edge.to, min(want, edge.capacity-edge.flow))
				if sent == 0 {
					continue
				}
				n.edges[ref].flow += sent
				n.edges[edge.rev].flow -= sent
				return sent
			}
			return 0
		}
		for {
			// A zero cap asks for the full maximum, so augment without an
			// artificial per-call ceiling; capacities stay within int64.
			want := int64(math.MaxInt64)
			if cap > 0 {
				want = cap - total
			}
			sent := push(source, want)
			if sent == 0 {
				break
			}
			total += sent
			if cap > 0 && total >= cap {
				return total
			}
		}
	}
	return total
}

func (n *flowNetwork) reachable(source int) []bool {
	seen := make([]bool, len(n.head))
	seen[source] = true
	queue := []int{source}
	for len(queue) > 0 {
		node := queue[0]
		queue = queue[1:]
		for _, ref := range n.head[node] {
			edge := n.edges[ref]
			if edge.capacity-edge.flow > 0 && !seen[edge.to] {
				seen[edge.to] = true
				queue = append(queue, edge.to)
			}
		}
	}
	return seen
}
