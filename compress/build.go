package compress

import (
	"container/heap"
	"slices"
	"strconv"

	"rillweave/graph"
	"rillweave/index"
)

// Request selects a fixed revision, the arc kinds forming the ring clusters and
// the exact-tier endpoint set that the installed summary must answer precisely.
type Request struct {
	Name      string   `json:"name"`
	Kinds     []string `json:"kinds,omitempty"`
	Endpoints []string `json:"endpoints"`
	Budget    int      `json:"budget,omitempty"`
}

type selected struct {
	arcs      []graph.Arc // every selected arc, ordered by arc ID
	out       map[string][]graph.Arc
	in        map[string][]graph.Arc
	kinds     map[string]bool
	allowance int
	scanned   int
}

// Build compresses one immutable graph revision into a Document. The work is
// bounded by the request budget, counted in candidate arc inspections exactly
// like the read-side topology.
func Build(g *graph.Graph, adj *index.Adjacency, req Request) (*Document, error) {
	if !graph.ValidID(req.Name) {
		return nil, graph.Fail(graph.Invalid, "invalid compressed graph name")
	}
	if len(req.Endpoints) == 0 || len(req.Endpoints) > MaxEndpoints {
		return nil, graph.Fail(graph.Invalid, "endpoint set must contain 1..%d origin vertex IDs", MaxEndpoints)
	}
	if len(req.Kinds) > graph.MaxRules {
		return nil, graph.Fail(graph.Invalid, "too many selected arc kinds")
	}
	kinds := make(map[string]bool, len(req.Kinds))
	for _, kind := range req.Kinds {
		if _, ok := g.Schema.Arcs[kind]; !ok {
			return nil, graph.Fail(graph.Invalid, "unknown arc kind %q", kind)
		}
		kinds[kind] = true
	}
	seenEndpoint := make(map[string]bool, len(req.Endpoints))
	for _, id := range req.Endpoints {
		if _, ok := g.Vertices[id]; !ok {
			return nil, graph.Fail(graph.Missing, "endpoint vertex %q is not in this revision", id)
		}
		if seenEndpoint[id] {
			return nil, graph.Fail(graph.Invalid, "endpoint %q is listed more than once", id)
		}
		seenEndpoint[id] = true
	}
	budget := req.Budget
	if budget == 0 {
		budget = DefaultBuildBudget
	}
	if budget < 1 || budget > MaxBuildBudget {
		return nil, graph.Fail(graph.Invalid, "budget must be in [1,%d]", MaxBuildBudget)
	}

	sel := &selected{
		out: make(map[string][]graph.Arc), in: make(map[string][]graph.Arc),
		kinds: kinds, allowance: budget,
	}
	vertexIDs := graph.Keys(g.Vertices)
	for _, id := range vertexIDs {
		if err := adj.Visit(id, index.Out, func(arc graph.Arc, next string) error {
			if sel.scanned == sel.allowance {
				return graph.Fail(graph.Budget, "compression budget exhausted after %d candidate arcs", sel.scanned)
			}
			sel.scanned++
			if len(kinds) != 0 && !kinds[arc.Kind] {
				return nil
			}
			sel.arcs = append(sel.arcs, arc)
			sel.out[arc.From] = append(sel.out[arc.From], arc)
			sel.in[next] = append(sel.in[next], arc)
			return nil
		}); err != nil {
			return nil, err
		}
	}

	membership, members, err := sel.components(vertexIDs)
	if err != nil {
		return nil, err
	}

	boundary := make(map[string]map[string]bool)
	for id := range members {
		boundary[id] = map[string]bool{}
	}
	for _, id := range req.Endpoints {
		boundary[membership[id]][id] = true
	}
	cross := []CrossArc{}
	for _, arc := range sel.arcs {
		fromGroup, toGroup := membership[arc.From], membership[arc.To]
		if fromGroup != toGroup {
			cross = append(cross, CrossArc{
				ArcID: arc.ID, Kind: arc.Kind, From: arc.From, To: arc.To,
				FromGroup: fromGroup, ToGroup: toGroup, Cost: arc.Cost,
			})
			boundary[fromGroup][arc.From] = true
			boundary[toGroup][arc.To] = true
		}
	}

	doc := &Document{
		Name: req.Name, SourceName: g.Name, SourceRevision: g.Revision,
		Kinds:     append([]string{}, req.Kinds...),
		Endpoints: append([]string{}, req.Endpoints...),
		CrossArcs: cross,
		Witnesses: []Witness{},
		Summaries: []SummaryEdge{},
	}
	retained := make(map[string]graph.Arc)
	for _, arc := range cross {
		retained[arc.ArcID] = g.Arcs[arc.ArcID]
	}

	// Shared, deduplicated witness pool keyed by the exact arc-ID sequence.
	// Only routes touching an agreed endpoint are retained as exact witnesses;
	// every other boundary pair remains queryable on the summary at overview
	// tier whenever a retained route happens to connect it.
	witnessByKey := make(map[string]Witness)
	edgeCount := 0
	witnessUse := make(map[string]int)
	for groupID, group := range members {
		groupBounds := graph.Keys(boundary[groupID])
		doc.Clusters = append(doc.Clusters, Cluster{
			ID: groupID, Boundaries: groupBounds, MemberCount: len(group),
		})
		if len(group) == 1 {
			continue
		}
		internal := make(map[string][]graph.Arc)
		for _, v := range group {
			for _, arc := range sel.out[v] {
				if membership[arc.To] == groupID {
					internal[v] = append(internal[v], arc)
				}
			}
		}
		edges, used, err := sel.clusterWitnesses(groupID, groupBounds, seenEndpoint, internal, witnessByKey)
		if err != nil {
			return nil, err
		}
		for i := range edges {
			edges[i].ID = edgeID(edgeCount)
			edgeCount++
		}
		doc.Summaries = append(doc.Summaries, edges...)
		for arcID, count := range used {
			witnessUse[arcID] += count
			retained[arcID] = g.Arcs[arcID]
		}
	}
	for _, witness := range witnessByKey {
		doc.Witnesses = append(doc.Witnesses, witness)
	}
	for _, arc := range retained {
		doc.ArcDict = append(doc.ArcDict, arcRecord(arc))
	}

	finalize(doc, witnessUse, len(vertexIDs), len(sel.arcs))
	return doc, nil
}

// components runs Kosaraju's algorithm with the same forward/reverse passes and
// smallest-ID naming used by the read-side components workflow.
func (s *selected) components(vertexIDs []string) (map[string]string, map[string][]string, error) {
	order := make([]string, 0, len(vertexIDs))
	seen := make(map[string]bool, len(vertexIDs))
	var dfsOut func(string) error
	dfsOut = func(id string) error {
		if seen[id] {
			return nil
		}
		seen[id] = true
		for _, arc := range s.out[id] {
			if err := s.charge(); err != nil {
				return err
			}
			if err := dfsOut(arc.To); err != nil {
				return err
			}
		}
		order = append(order, id)
		return nil
	}
	for _, id := range vertexIDs {
		if err := dfsOut(id); err != nil {
			return nil, nil, err
		}
	}
	clear(seen)
	membership := make(map[string]string, len(vertexIDs))
	members := make(map[string][]string)
	var dfsIn func(string, string) error
	dfsIn = func(id, group string) error {
		if seen[id] {
			return nil
		}
		seen[id] = true
		membership[id] = group
		members[group] = append(members[group], id)
		for _, arc := range s.in[id] {
			if err := s.charge(); err != nil {
				return err
			}
			if err := dfsIn(arc.From, group); err != nil {
				return err
			}
		}
		return nil
	}
	for i := len(order) - 1; i >= 0; i-- {
		id := order[i]
		if seen[id] {
			continue
		}
		if err := dfsIn(id, id); err != nil {
			return nil, nil, err
		}
	}
	return membership, members, nil
}

func (s *selected) charge() error {
	if s.scanned == s.allowance {
		return graph.Fail(graph.Budget, "compression budget exhausted after %d candidate arcs", s.scanned)
	}
	s.scanned++
	return nil
}

// routeLabel orders candidates with the original path contract: minimum total
// cost, then fewest original arcs, then the lexicographically smallest arc ID
// sequence.
type routeLabel struct {
	vertex string
	cost   int64
	arcs   []string
	index  int
}

type labelHeap []*routeLabel

func (h labelHeap) Len() int { return len(h) }
func (h labelHeap) Less(i, j int) bool {
	a, b := h[i], h[j]
	if a.cost != b.cost {
		return a.cost < b.cost
	}
	if len(a.arcs) != len(b.arcs) {
		return len(a.arcs) < len(b.arcs)
	}
	return slices.Compare(a.arcs, b.arcs) < 0
}
func (h labelHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i]; h[i].index, h[j].index = j, i }
func (h *labelHeap) Push(x any) {
	label := x.(*routeLabel)
	label.index = len(*h)
	*h = append(*h, label)
}
func (h *labelHeap) Pop() any {
	old := *h
	n := len(old)
	label := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return label
}

func labelLess(a, b *routeLabel) bool {
	if a.cost != b.cost {
		return a.cost < b.cost
	}
	if len(a.arcs) != len(b.arcs) {
		return len(a.arcs) < len(b.arcs)
	}
	return slices.Compare(a.arcs, b.arcs) < 0
}

// clusterWitnesses computes the best route from every boundary vertex to every
// other boundary vertex using only arcs internal to the cluster. Only pairs
// touching an agreed endpoint are stored as exact witnesses; the others are
// intentionally omitted so internal ring arcs unrelated to an agreed query are
// not carried in the portable image. Equal arc-ID sequences share one pool
// entry, including the common ring prefixes reused by several endpoint pairs.
func (s *selected) clusterWitnesses(group string, bounds []string, endpoints map[string]bool,
	internal map[string][]graph.Arc, pool map[string]Witness,
) ([]SummaryEdge, map[string]int, error) {
	usage := map[string]int{}
	edges := []SummaryEdge{}
	for _, source := range bounds {
		root := &routeLabel{vertex: source, cost: 0, arcs: []string{}}
		dist := map[string]*routeLabel{source: root}
		queue := &labelHeap{root}
		heap.Init(queue)
		for queue.Len() != 0 {
			cur := heap.Pop(queue).(*routeLabel)
			if dist[cur.vertex] != cur {
				continue // stale heap entry
			}
			for _, arc := range internal[cur.vertex] {
				if err := s.charge(); err != nil {
					return nil, nil, err
				}
				candidate := &routeLabel{
					vertex: arc.To, cost: cur.cost + arc.Cost,
					arcs: append(slices.Clone(cur.arcs), arc.ID),
				}
				prior, ok := dist[arc.To]
				if ok && !labelLess(candidate, prior) {
					continue
				}
				dist[arc.To] = candidate
				heap.Push(queue, candidate)
			}
		}
		for _, target := range bounds {
			if target == source || !(endpoints[source] || endpoints[target]) {
				continue
			}
			label, ok := dist[target]
			if !ok {
				continue // boundary pair unreachable inside this cluster
			}
			key := seqKey(label.arcs)
			witness, exists := pool[key]
			if !exists {
				witness = Witness{
					ID: witnessID(len(pool)), ArcIDs: slices.Clone(label.arcs),
					Cost: label.cost, Hops: len(label.arcs),
				}
				pool[key] = witness
				for _, arcID := range label.arcs {
					usage[arcID]++
				}
			}
			edges = append(edges, SummaryEdge{
				Cluster: group, From: source, To: target,
				WitnessID: witness.ID, Cost: label.cost, Hops: len(label.arcs),
			})
		}
	}
	return edges, usage, nil
}

func edgeID(i int) string    { return "e-" + strconv.Itoa(i) }
func witnessID(i int) string { return "w-" + strconv.Itoa(i) }

func seqKey(arcs []string) string {
	key := make([]byte, 0, len(arcs)*8)
	for _, id := range arcs {
		key = append(key, id...)
		key = append(key, 0)
	}
	return string(key)
}

func arcRecord(a graph.Arc) ArcRecord {
	return ArcRecord{ID: a.ID, Kind: a.Kind, From: a.From, To: a.To, Cost: a.Cost}
}

func finalize(doc *Document, witnessUse map[string]int, originalVertices, selectedArcs int) {
	slices.SortFunc(doc.Clusters, func(a, b Cluster) int { return compareStrings(a.ID, b.ID) })
	for i := range doc.Clusters {
		slices.Sort(doc.Clusters[i].Boundaries)
	}
	slices.SortFunc(doc.CrossArcs, func(a, b CrossArc) int { return compareStrings(a.ArcID, b.ArcID) })
	slices.SortFunc(doc.Witnesses, func(a, b Witness) int { return compareStrings(a.ID, b.ID) })
	slices.SortFunc(doc.Summaries, func(a, b SummaryEdge) int {
		if n := compareStrings(a.Cluster, b.Cluster); n != 0 {
			return n
		}
		if n := compareStrings(a.From, b.From); n != 0 {
			return n
		}
		return compareStrings(a.To, b.To)
	})
	slices.SortFunc(doc.ArcDict, func(a, b ArcRecord) int { return compareStrings(a.ID, b.ID) })
	slices.Sort(doc.Kinds)
	slices.Sort(doc.Endpoints)

	// A witness is shared when another summary edge references it too, or when
	// its arcs participate in multiple distinct witness sequences.
	referenceCount := make(map[string]int)
	for _, edge := range doc.Summaries {
		referenceCount[edge.WitnessID]++
	}
	witnessByID := make(map[string]Witness, len(doc.Witnesses))
	for _, witness := range doc.Witnesses {
		witnessByID[witness.ID] = witness
	}
	shared := 0
	for i := range doc.Summaries {
		witness := witnessByID[doc.Summaries[i].WitnessID]
		usedByOthers := referenceCount[doc.Summaries[i].WitnessID] > 1
		if !usedByOthers {
			for _, arcID := range witness.ArcIDs {
				if witnessUse[arcID] > 1 {
					usedByOthers = true
					break
				}
			}
		}
		if usedByOthers {
			doc.Summaries[i].Shared = true
			shared++
		}
	}
	retainedVertices := 0
	for _, cluster := range doc.Clusters {
		retainedVertices += len(cluster.Boundaries)
	}
	trivial := 0
	for _, cluster := range doc.Clusters {
		if cluster.MemberCount == 1 {
			trivial++
		}
	}
	doc.Stats = Stats{
		OriginalVertices: originalVertices,
		OriginalArcs:     selectedArcs,
		RetainedVertices: retainedVertices,
		RetainedArcs:     len(doc.ArcDict),
		Clusters:         len(doc.Clusters),
		TrivialClusters:  trivial,
		CrossArcs:        len(doc.CrossArcs),
		SummaryEdges:     len(doc.Summaries),
		Witnesses:        len(doc.Witnesses),
		SharedWitnesses:  shared,
	}
	if selectedArcs > 0 {
		doc.Stats.ArcRatio = float64(len(doc.ArcDict)) / float64(selectedArcs)
	}
}

func compareStrings(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}
