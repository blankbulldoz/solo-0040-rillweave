package compress

import (
	"container/heap"
	"slices"

	"rillweave/graph"
)

// RouteRequest asks for the cheapest forward walk between two origin vertex
// IDs on the installed summary. A positive MaxHops downgrades the answer to
// the overview tier because hop-bounded semantics were not part of the exact
// export contract.
type RouteRequest struct {
	From    string `json:"from"`
	To      string `json:"to"`
	MaxHops int    `json:"max_hops,omitempty"`
	Budget  int    `json:"budget,omitempty"`
}

// RouteAnswer is the exact or overview result of a summary route query.
type RouteAnswer struct {
	Found       bool     `json:"found"`
	Exact       bool     `json:"exact"`
	Tier        string   `json:"tier"`
	Note        string   `json:"note,omitempty"`
	Cost        int64    `json:"cost,omitempty"`
	Vertices    []string `json:"vertices,omitempty"`
	Arcs        []string `json:"arcs,omitempty"`
	SummaryHops int      `json:"summary_hops,omitempty"`
	Scanned     int      `json:"scanned"`
}

// Verification is the offline replay of a witness against the retained arc
// dictionary. It needs no access to the source instance.
type Verification struct {
	Valid        bool     `json:"valid"`
	Cost         int64    `json:"cost"`
	Vertices     []string `json:"vertices"`
	Arcs         []string `json:"arcs"`
	FirstProblem string   `json:"first_problem,omitempty"`
}

// Network is the in-memory query form of a document, rebuilt on every restart.
type Network struct {
	doc       *Document
	clusterOf map[string]string
	endpoint  map[string]bool
	adj       map[string][]networkEdge
	arcs      map[string]ArcRecord
	witnesses map[string]Witness
}

type networkEdge struct {
	// summary edge ID ("e-..") or cross arc ID ("c:<id>")
	id       string
	arcIDs   []string
	to       string
	cost     int64
	original int // number of original arcs the edge expands to
}

// Materialize validates the document and builds the boundary-level network.
func Materialize(doc *Document) (*Network, error) {
	if err := Validate(doc); err != nil {
		return nil, err
	}
	n := &Network{
		doc:       doc,
		clusterOf: make(map[string]string),
		endpoint:  make(map[string]bool, len(doc.Endpoints)),
		adj:       make(map[string][]networkEdge),
		arcs:      make(map[string]ArcRecord, len(doc.ArcDict)),
		witnesses: make(map[string]Witness, len(doc.Witnesses)),
	}
	for _, endpoint := range doc.Endpoints {
		n.endpoint[endpoint] = true
	}
	for _, record := range doc.ArcDict {
		n.arcs[record.ID] = record
	}
	for _, witness := range doc.Witnesses {
		n.witnesses[witness.ID] = witness
	}
	for _, cluster := range doc.Clusters {
		for _, vertex := range cluster.Boundaries {
			n.clusterOf[vertex] = cluster.ID
			n.adj[vertex] = []networkEdge{}
		}
	}
	for _, edge := range doc.Summaries {
		witness := n.witnesses[edge.WitnessID]
		n.adj[edge.From] = append(n.adj[edge.From], networkEdge{
			id: edge.ID, arcIDs: slices.Clone(witness.ArcIDs), to: edge.To,
			cost: edge.Cost, original: edge.Hops,
		})
	}
	for _, arc := range doc.CrossArcs {
		n.adj[arc.From] = append(n.adj[arc.From], networkEdge{
			id: "c:" + arc.ArcID, arcIDs: []string{arc.ArcID}, to: arc.To,
			cost: arc.Cost, original: 1,
		})
	}
	for vertex := range n.adj {
		slices.SortFunc(n.adj[vertex], func(a, b networkEdge) int {
			if a.cost != b.cost {
				if a.cost < b.cost {
					return -1
				}
				return 1
			}
			if a.original != b.original {
				return a.original - b.original
			}
			return slices.Compare(a.arcIDs, b.arcIDs)
		})
	}
	return n, nil
}

// Doc exposes the validated document behind a materialized network.
func (n *Network) Doc() *Document { return n.doc }

type queryLabel struct {
	vertex   string
	cost     int64
	arcs     []string // original arc IDs of the full walk so far
	edges    []string // summary-level edge IDs, used for deterministic ties
	original int
	index    int
}

type queryHeap []*queryLabel

func (h queryHeap) Len() int { return len(h) }
func (h queryHeap) Less(i, j int) bool {
	a, b := h[i], h[j]
	if a.cost != b.cost {
		return a.cost < b.cost
	}
	if a.original != b.original {
		return a.original < b.original
	}
	return slices.Compare(a.edges, b.edges) < 0
}
func (h queryHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = j, i
}
func (h *queryHeap) Push(x any) {
	label := x.(*queryLabel)
	label.index = len(*h)
	*h = append(*h, label)
}
func (h *queryHeap) Pop() any {
	old := *h
	m := len(old)
	label := old[m-1]
	old[m-1] = nil
	*h = old[:m-1]
	return label
}

func queryLess(a, b *queryLabel) bool {
	if a.cost != b.cost {
		return a.cost < b.cost
	}
	if a.original != b.original {
		return a.original < b.original
	}
	return slices.Compare(a.edges, b.edges) < 0
}

const (
	defaultRouteBudget = 300000
	maxRouteBudget     = 6000000
	tierExact          = "exact"
	tierOverview       = "overview"
)

// Route answers one forward minimum-cost query on the summary network.
func (n *Network) Route(req RouteRequest) (RouteAnswer, error) {
	if req.MaxHops < 0 || req.MaxHops > 32 {
		return RouteAnswer{}, graph.Fail(graph.Invalid, "max_hops must be in [0,32]")
	}
	budget := req.Budget
	if budget == 0 {
		budget = defaultRouteBudget
	}
	if budget < 1 || budget > maxRouteBudget {
		return RouteAnswer{}, graph.Fail(graph.Invalid, "budget must be in [1,%d]", maxRouteBudget)
	}
	fromCluster, fromKnown := n.clusterOf[req.From]
	toCluster, toKnown := n.clusterOf[req.To]
	if !fromKnown || !toKnown {
		missing := req.From
		if toKnown {
			missing = req.To
		}
		return RouteAnswer{}, graph.Fail(graph.Missing, "vertex %q is not retained by this compressed graph", missing)
	}

	exact := n.endpoint[req.From] && n.endpoint[req.To] &&
		fromCluster != toCluster && req.MaxHops == 0
	answer := RouteAnswer{Found: false, Exact: exact}
	if exact {
		answer.Tier = tierExact
	} else {
		answer.Tier = tierOverview
		switch {
		case fromCluster == toCluster:
			answer.Note = "same-cluster pair: only cross-cluster endpoint pairs have exact export guarantees"
		case req.MaxHops > 0:
			answer.Note = "hop-bounded request: original hop limits are retained as overview metadata only"
		default:
			answer.Note = "endpoint outside the agreed exact set: cost and witness are complete on the summary but not contractually exact"
		}
	}
	if req.From == req.To {
		answer.Found, answer.Exact = true, false
		answer.Tier, answer.Note = tierOverview, "zero-length walk at one vertex"
		answer.Vertices, answer.Arcs = []string{req.From}, []string{}
		return answer, nil
	}

	root := &queryLabel{vertex: req.From, arcs: []string{}, edges: []string{}}
	dist := map[string]*queryLabel{req.From: root}
	queue := &queryHeap{root}
	heap.Init(queue)
	scanned := 0
	for queue.Len() != 0 {
		cur := heap.Pop(queue).(*queryLabel)
		if dist[cur.vertex] != cur {
			continue
		}
		if cur.vertex == req.To {
			answer.Found = true
			answer.Cost = cur.cost
			answer.Arcs = cur.arcs
			answer.SummaryHops = len(cur.edges)
			answer.Vertices = n.replayVertices(cur.arcs, req.From)
			answer.Scanned = scanned
			return answer, nil
		}
		for _, edge := range n.adj[cur.vertex] {
			if scanned == budget {
				return RouteAnswer{}, graph.Fail(graph.Budget, "route budget exhausted after %d summary edges", scanned)
			}
			scanned++
			candidate := &queryLabel{
				vertex: edge.to, cost: cur.cost + edge.cost,
				arcs:     append(slices.Clone(cur.arcs), edge.arcIDs...),
				edges:    append(slices.Clone(cur.edges), edge.id),
				original: cur.original + edge.original,
			}
			prior, ok := dist[edge.to]
			if ok && !queryLess(candidate, prior) {
				continue
			}
			dist[edge.to] = candidate
			heap.Push(queue, candidate)
		}
	}
	answer.Scanned = scanned
	if exact {
		answer.Note = "definitive no-route: no forward walk exists between the agreed endpoints in the source revision"
	}
	return answer, nil
}

// Replay walks the retained original arc records in order, proving that a
// witness is contiguous and reporting its realized total cost. The source
// graph instance is not consulted.
func (n *Network) Replay(arcIDs []string) Verification {
	out := Verification{Arcs: slices.Clone(arcIDs), Vertices: []string{}}
	if len(arcIDs) == 0 {
		out.FirstProblem = "empty witness has no starting vertex; replay requires the origin vertex separately"
		return out
	}
	var total int64
	var vertex string
	for i, id := range arcIDs {
		record, ok := n.arcs[id]
		if !ok {
			out.FirstProblem = "arc " + id + " is absent from the retained arc dictionary"
			return out
		}
		if i == 0 {
			vertex = record.From
			out.Vertices = append(out.Vertices, vertex)
		} else if record.From != vertex {
			out.FirstProblem = "arc " + id + " does not continue from " + vertex
			return out
		}
		vertex = record.To
		total += record.Cost
		out.Vertices = append(out.Vertices, vertex)
	}
	out.Valid, out.Cost = true, total
	return out
}

func (n *Network) replayVertices(arcIDs []string, start string) []string {
	vertices := []string{start}
	vertex := start
	for _, id := range arcIDs {
		record := n.arcs[id]
		if record.From != vertex {
			return []string{} // a validated network never reaches this branch
		}
		vertex = record.To
		vertices = append(vertices, vertex)
	}
	return vertices
}
