package walk

import (
	"cmp"
	"slices"

	"rillweave/graph"
	"rillweave/index"
)

// TriadSlots are the three named vertex variables of a triangle pattern.
var TriadSlots = [3]string{"a", "b", "c"}

// Scalar re-exports the graph scalar so the walk package owns its request
// shape while the wire layer still decodes the same typed value.
type Scalar = graph.Scalar

// VertexPattern binds one vertex variable to a kind and a conjunction of
// typed property-equality predicates.
type VertexPattern struct {
	Kind       string            `json:"kind"`
	Properties map[string]Scalar `json:"properties,omitempty"`
}

// EdgePattern binds one directed arc variable between two vertex variables.
// Direction follows the stored arc: "out" requires From->To, "in" the
// reverse; the witnessing arc keeps its stored identity and cost.
type EdgePattern struct {
	Kind      string          `json:"kind"`
	From      string          `json:"from"`
	To        string          `json:"to"`
	Direction index.Direction `json:"direction,omitempty"`
}

type TriadRequest struct {
	Vertices map[string]VertexPattern `json:"vertices"`
	Edges    []EdgePattern            `json:"edges"`
	Limit    int                      `json:"limit,omitempty"`
	Budget   int                      `json:"budget,omitempty"`
}

type TriadRow struct {
	Vertices map[string]string `json:"vertices"`
	Arcs     map[string]string `json:"arcs"`
}

type TriadResult struct {
	Rows       []TriadRow `json:"rows"`
	Matched    int        `json:"matched"`
	Truncated  bool       `json:"truncated"`
	Exhaustive bool       `json:"exhaustive"`
	Scanned    int        `json:"scanned"`
}

type triadPlan struct {
	vertexRoot bool
	root       int
	cost       uint64
}

// estimateSaturate caps planner arithmetic: any estimate at or above it is
// treated as "effectively unbounded", which keeps wide intermediate joins
// from overflowing while remaining far above real budgets.
const estimateSaturate uint64 = 1 << 40

func Triad(g *graph.Graph, adj *index.Adjacency, props *index.Postings, q TriadRequest) (TriadResult, error) {
	limit, err := Limit(q.Limit)
	if err != nil {
		return TriadResult{}, err
	}
	budget := q.Budget
	if budget == 0 {
		budget = MaxBudget
	}
	if budget < 1 || budget > MaxBudget {
		return TriadResult{}, graph.Fail(graph.Invalid, "budget must be in [1,%d]", MaxBudget)
	}
	patterns, edges, edgeSlot, err := validateTriad(g, q)
	if err != nil {
		return TriadResult{}, err
	}
	domains, err := vertexDomains(g, props, patterns)
	if err != nil {
		return TriadResult{}, err
	}
	t := &topology{adj: adj, allowance: budget}
	plan := choosePlan(g, adj, edges, domains)
	exec := &triadExec{
		t: t, edges: edges, edgeSlot: edgeSlot, plan: plan,
		limit: limit, usedArcs: map[string]bool{},
		result: TriadResult{Rows: []TriadRow{}, Exhaustive: true},
	}
	if plan.vertexRoot {
		err = exec.runVertexRoot(domains)
	} else {
		err = exec.runEdgeRoot(domains)
	}
	if err != nil {
		return TriadResult{}, err
	}
	sortRows(exec.result.Rows)
	exec.result.Scanned = t.scanned
	exec.result.Truncated = exec.result.Matched > limit
	return exec.result, nil
}

// sortRows gives a stable, total order across complete witnesses: the three
// vertex IDs first, then the three arc IDs, each in fixed slot order.
func sortRows(rows []TriadRow) {
	slices.SortFunc(rows, func(a, b TriadRow) int {
		for _, slot := range TriadSlots {
			if n := cmp.Compare(a.Vertices[slot], b.Vertices[slot]); n != 0 {
				return n
			}
		}
		for i := range [3]int{} {
			if n := cmp.Compare(a.Arcs[edgeName(i)], b.Arcs[edgeName(i)]); n != 0 {
				return n
			}
		}
		return 0
	})
}

func validateTriad(g *graph.Graph, q TriadRequest) ([3]VertexPattern, []EdgePattern, map[int][2]int, error) {
	var patterns [3]VertexPattern
	if len(q.Vertices) != 3 {
		return patterns, nil, nil, graph.Fail(graph.Invalid, "a triad requires exactly three vertex variables a, b and c")
	}
	for slot, name := range TriadSlots {
		pattern, ok := q.Vertices[name]
		if !ok {
			return patterns, nil, nil, graph.Fail(graph.Invalid, "missing vertex variable %q", name)
		}
		rule, ok := g.Schema.Vertices[pattern.Kind]
		if !ok {
			return patterns, nil, nil, graph.Fail(graph.Invalid, "unknown vertex kind %q", pattern.Kind)
		}
		if len(pattern.Properties) > graph.MaxProps {
			return patterns, nil, nil, graph.Fail(graph.Invalid, "vertex %q has too many property predicates", name)
		}
		for property, value := range pattern.Properties {
			field, ok := rule.Properties[property]
			if !ok {
				return patterns, nil, nil, graph.Fail(graph.Invalid, "unknown property %q on vertex kind %q", property, pattern.Kind)
			}
			if field.Kind != value.Kind || !value.Valid() {
				return patterns, nil, nil, graph.Fail(graph.Invalid, "property %q requires %s", property, field.Kind)
			}
		}
		patterns[slot] = pattern
	}
	if len(q.Edges) != 3 {
		return patterns, nil, nil, graph.Fail(graph.Invalid, "a triad requires exactly three edge variables")
	}
	edges := make([]EdgePattern, 3)
	pairs := map[[2]int]bool{}
	edgeSlot := map[int][2]int{}
	for i, edge := range q.Edges {
		from, okFrom := slotIndex(edge.From)
		to, okTo := slotIndex(edge.To)
		if !okFrom || !okTo {
			return patterns, nil, nil, graph.Fail(graph.Invalid, "edge %d must reference vertex variables a, b or c", i)
		}
		if _, ok := g.Schema.Arcs[edge.Kind]; !ok {
			return patterns, nil, nil, graph.Fail(graph.Invalid, "unknown arc kind %q", edge.Kind)
		}
		direction := edge.Direction
		if direction == "" {
			direction = index.Out
		}
		if direction != index.Out && direction != index.In {
			return patterns, nil, nil, graph.Fail(graph.Invalid, "edge direction must be out or in")
		}
		pair := orderedPair(from, to)
		if pairs[pair] {
			return patterns, nil, nil, graph.Fail(graph.Invalid, "edges must bind the three distinct endpoint pairs of a triangle")
		}
		pairs[pair] = true
		edge.Direction = direction
		edges[i] = edge
		edgeSlot[i] = pair
	}
	return patterns, edges, edgeSlot, nil
}

func slotIndex(name string) (int, bool) {
	for i, slot := range TriadSlots {
		if slot == name {
			return i, true
		}
	}
	return 0, false
}

func orderedPair(a, b int) [2]int {
	if a <= b {
		return [2]int{a, b}
	}
	return [2]int{b, a}
}

// vertexDomains resolves every variable to its sorted candidate IDs. Kind
// membership is always enforced, then each typed equality narrows through
// the immutable posting for that (kind, property, value).
func vertexDomains(g *graph.Graph, props *index.Postings, patterns [3]VertexPattern) ([3][]string, error) {
	var domains [3][]string
	for slot, pattern := range patterns {
		candidates := map[string]bool{}
		propertyKeys := graph.Keys(pattern.Properties)
		if len(propertyKeys) == 0 {
			for id, vertex := range g.Vertices {
				if vertex.Kind == pattern.Kind {
					candidates[id] = true
				}
			}
		} else {
			for i, property := range propertyKeys {
				ids := props.Matches(pattern.Kind, property, pattern.Properties[property])
				if i == 0 {
					for _, id := range ids {
						candidates[id] = true
					}
				} else {
					next := make(map[string]bool, len(ids))
					for _, id := range ids {
						if candidates[id] {
							next[id] = true
						}
					}
					candidates = next
				}
			}
		}
		ids := make([]string, 0, len(candidates))
		for id := range candidates {
			// Postings are kind-scoped but keep the guard explicit so a
			// candidate can never satisfy the wrong vertex type.
			if g.Vertices[id].Kind == pattern.Kind {
				ids = append(ids, id)
			}
		}
		slices.Sort(ids)
		domains[slot] = ids
	}
	return domains, nil
}

// choosePlan builds one vertex-rooted and one edge-rooted plan and keeps
// the cheaper estimate. A wide vertex condition therefore never forces a
// large first enumeration when a sparse arc kind can seed the query.
func choosePlan(g *graph.Graph, adj *index.Adjacency, edges []EdgePattern, domains [3][]string) triadPlan {
	vertexPlans := make([]triadPlan, 0, 3)
	for slot := range TriadSlots {
		cost := uint64(max(1, len(domains[slot])))
		for _, edge := range edges {
			cost = saturate(cost * uint64(edgeDegree(g, adj, edge.Kind)))
		}
		vertexPlans = append(vertexPlans, triadPlan{vertexRoot: true, root: slot, cost: cost})
	}
	slices.SortFunc(vertexPlans, func(a, b triadPlan) int {
		if n := cmp.Compare(a.cost, b.cost); n != 0 {
			return n
		}
		return cmp.Compare(a.root, b.root)
	})
	edgePlans := make([]triadPlan, 0, 3)
	for i, edge := range edges {
		// Seed with the root kind's own arc count, then multiply the
		// per-vertex incidence degree of every other edge so a rare closing
		// kind outranks a wide one even when the wide kind appears twice.
		cost := uint64(max(1, len(adj.ArcsOfKind(edge.Kind))))
		for j, other := range edges {
			if j == i {
				continue
			}
			cost = saturate(cost * uint64(edgeDegree(g, adj, other.Kind)))
		}
		edgePlans = append(edgePlans, triadPlan{vertexRoot: false, root: i, cost: cost})
	}
	slices.SortFunc(edgePlans, func(a, b triadPlan) int {
		if n := cmp.Compare(a.cost, b.cost); n != 0 {
			return n
		}
		return cmp.Compare(a.root, b.root)
	})
	if edgePlans[0].cost < vertexPlans[0].cost {
		return edgePlans[0]
	}
	return vertexPlans[0]
}

func edgeDegree(g *graph.Graph, adj *index.Adjacency, kind string) int {
	arcs := adj.ArcsOfKind(kind)
	if len(arcs) == 0 {
		return 1
	}
	incidence := make(map[string]int, len(arcs)*2)
	for _, arc := range arcs {
		incidence[arc.From]++
		if arc.To != arc.From {
			incidence[arc.To]++
		}
	}
	degree := 1
	for id := range incidence {
		if _, ok := g.Vertices[id]; ok {
			degree = max(degree, incidence[id])
		}
	}
	return degree
}

func saturate(n uint64) uint64 {
	if n > estimateSaturate {
		return estimateSaturate
	}
	return n
}

type triadExec struct {
	t        *topology
	edges    []EdgePattern
	edgeSlot map[int][2]int
	plan     triadPlan
	limit    int
	result   TriadResult

	vertexIDs [3]string
	bound     [3]bool
	arcIDs    [3]string
	usedArcs  map[string]bool
}

func (x *triadExec) runVertexRoot(domains [3][]string) error {
	x.bound[x.plan.root] = true
	for _, id := range domains[x.plan.root] {
		x.vertexIDs[x.plan.root] = id
		if err := x.extend(domains); err != nil {
			return err
		}
	}
	return nil
}

func (x *triadExec) runEdgeRoot(domains [3][]string) error {
	edge := x.plan.root
	from, to := x.edgeEndpoints(edge)
	for _, arc := range x.t.adj.ArcsOfKind(x.edges[edge].Kind) {
		if err := x.t.charge(); err != nil {
			return err
		}
		source, target := x.orient(edge, arc)
		if x.bound[from] && x.vertexIDs[from] != source {
			continue
		}
		if x.bound[to] && x.vertexIDs[to] != target {
			continue
		}
		if !x.bindRootArc(edge, arc, domains) {
			continue
		}
		x.arcIDs[edge] = arc.ID
		x.usedArcs[arc.ID] = true
		err := x.extend(domains)
		delete(x.usedArcs, arc.ID)
		x.arcIDs[edge] = ""
		x.releaseRootEnds(edge)
		if err != nil {
			return err
		}
	}
	return nil
}

// bindRootArc fixes both endpoints of a root arc. It returns false when an
// endpoint contradicts its vertex domain. Self-loops bind a single slot.
func (x *triadExec) bindRootArc(edge int, arc graph.Arc, domains [3][]string) bool {
	from, to := x.edgeEndpoints(edge)
	source, target := x.orient(edge, arc)
	slots := []int{from}
	values := []string{source}
	if from != to {
		slots = append(slots, to)
		values = append(values, target)
	}
	for i, slot := range slots {
		id := values[i]
		if x.bound[slot] {
			if x.vertexIDs[slot] != id {
				return false
			}
			continue
		}
		if !slices.Contains(domains[slot], id) {
			return false
		}
		x.vertexIDs[slot] = id
		x.bound[slot] = true
	}
	return true
}

func (x *triadExec) releaseRootEnds(edge int) {
	from, to := x.edgeEndpoints(edge)
	x.bound[from] = false
	if from != to {
		x.bound[to] = false
	}
}

// extend binds one more edge. It chooses the lowest-index free edge that
// touches an already bound vertex, so a root edge can introduce the third
// vertex through adjacency. When all three arcs are bound and all three
// vertices are fixed, the complete witness is emitted.
func (x *triadExec) extend(domains [3][]string) error {
	if x.arcIDs[0] != "" && x.arcIDs[1] != "" && x.arcIDs[2] != "" {
		if x.bound[0] && x.bound[1] && x.bound[2] {
			x.emit()
		}
		return nil
	}
	edge := x.nextEdge()
	if edge < 0 {
		return nil
	}
	return x.joinEdge(edge, domains)
}

func (x *triadExec) joinEdge(edge int, domains [3][]string) error {
	from, to := x.edgeEndpoints(edge)
	freeSlot := -1
	root := -1
	switch {
	case x.bound[from] && x.bound[to]:
		// Closing edge: both endpoints already fixed.
	case !x.bound[from] && x.bound[to]:
		freeSlot, root = from, to
	case x.bound[from] && !x.bound[to]:
		freeSlot, root = to, from
	default:
		// No bound endpoint; another edge must bind a shared vertex first.
		return nil
	}
	return x.enumerate(edge, from, to, freeSlot, root, func(arc graph.Arc, candidate string) error {
		if x.usedArcs[arc.ID] {
			return nil
		}
		if freeSlot >= 0 && !slices.Contains(domains[freeSlot], candidate) {
			return nil
		}
		x.arcIDs[edge] = arc.ID
		x.usedArcs[arc.ID] = true
		if freeSlot >= 0 {
			x.vertexIDs[freeSlot] = candidate
			x.bound[freeSlot] = true
		}
		err := x.extend(domains)
		delete(x.usedArcs, arc.ID)
		x.arcIDs[edge] = ""
		if freeSlot >= 0 {
			x.bound[freeSlot] = false
		}
		return err
	})
}

// nextEdge returns the lowest-index free edge that touches a bound vertex.
func (x *triadExec) nextEdge() int {
	for edge := range x.edges {
		if x.arcIDs[edge] != "" {
			continue
		}
		from, to := x.edgeEndpoints(edge)
		if x.bound[from] || x.bound[to] {
			return edge
		}
	}
	return -1
}

func (x *triadExec) edgeEndpoints(edge int) (int, int) {
	pair := x.edgeSlot[edge]
	pattern := x.edges[edge]
	from, _ := slotIndex(pattern.From)
	if from == pair[0] {
		return pair[0], pair[1]
	}
	return pair[1], pair[0]
}

// orient maps a stored arc onto the edge's requested direction, returning
// the vertex that must sit at the edge's From and To slots.
func (x *triadExec) orient(edge int, arc graph.Arc) (string, string) {
	if x.edges[edge].Direction == index.Out {
		return arc.From, arc.To
	}
	return arc.To, arc.From
}

// enumerate walks the adjacency posting needed for one edge. freeSlot is -1
// when both endpoints are already fixed; otherwise it names the slot the
// neighbor fills and root the already-bound slot the walk starts from.
func (x *triadExec) enumerate(edge, from, to, freeSlot, root int, use func(graph.Arc, string) error) error {
	kind := x.edges[edge].Kind
	if freeSlot < 0 {
		// Both endpoints fixed: walk the From slot in the edge's direction
		// and keep only arcs reaching the already-bound To slot.
		return x.t.visitKind(x.vertexIDs[from], x.edges[edge].Direction, kind, func(arc graph.Arc, next string) error {
			if next != x.vertexIDs[to] {
				return nil
			}
			return use(arc, next)
		})
	}
	// A free From slot needs a reverse traversal; a free To slot a forward one.
	step := x.edges[edge].Direction
	if freeSlot == from {
		if step == index.Out {
			step = index.In
		} else {
			step = index.Out
		}
	}
	return x.t.visitKind(x.vertexIDs[root], step, kind, func(arc graph.Arc, next string) error {
		return use(arc, next)
	})
}

func (t *topology) charge() error {
	if t.scanned == t.allowance {
		return graph.Fail(graph.Budget, "expansion budget exhausted after %d candidate arcs", t.scanned)
	}
	t.scanned++
	return nil
}

// visitKind enumerates one adjacency posting, charging every inspected arc
// against the budget and admitting only the single kind required by the
// edge variable being bound.
func (t *topology) visitKind(id string, d index.Direction, kind string, use func(graph.Arc, string) error) error {
	return t.adj.Visit(id, d, func(arc graph.Arc, next string) error {
		if err := t.charge(); err != nil {
			return err
		}
		if arc.Kind != kind {
			return nil
		}
		return use(arc, next)
	})
}

func (x *triadExec) emit() {
	x.result.Matched++
	if x.result.Matched <= x.limit {
		row := TriadRow{Vertices: map[string]string{}, Arcs: map[string]string{}}
		for slot, name := range TriadSlots {
			row.Vertices[name] = x.vertexIDs[slot]
		}
		for i := range x.edges {
			row.Arcs[edgeName(i)] = x.arcIDs[i]
		}
		x.result.Rows = append(x.result.Rows, row)
	}
}

func edgeName(i int) string {
	return "e" + string(rune('1'+i))
}
