package change

import (
	"math"
	"slices"
	"strings"

	"rillweave/catalog"
	"rillweave/graph"
	"rillweave/index"
	"rillweave/walk"
)

const (
	MaxPurgeVertices = 2000
	MaxPurgePairs    = 1024
)

// Pair is one preserved dependency: From must remain able to reach To through
// arcs of the selected kinds walked in the selected direction.
type Pair struct {
	From      string          `json:"from"`
	To        string          `json:"to"`
	Kinds     []string        `json:"kinds,omitempty"`
	Direction index.Direction `json:"direction,omitempty"`
}

// Request describes a batch vertex removal and the pairs it must protect.
type Request struct {
	Vertices []string `json:"vertices"`
	Preserve []Pair   `json:"preserve,omitempty"`
	Budget   int      `json:"budget,omitempty"`
}

// Confirmation signs off a preview: the exact arc closure of the removal and
// the head revision the preview ran against.
type Confirmation struct {
	Base uint64   `json:"base"`
	Arcs []string `json:"arcs"`
	Request
}

// Status is the ruling for one preserved pair.
type Status string

const (
	Kept   Status = "kept"   // a route still exists once the batch is removed
	Broken Status = "broken" // reachable now, no route remains: blocks the batch
	Absent Status = "absent" // no route exists even now; reported, never blocks
)

// PairResult rules on one pair. Before lists the current witness arcs when a
// route exists; After lists the surviving witness arcs for a kept pair.
type PairResult struct {
	Pair
	Status Status   `json:"status"`
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
}

// Plan is the previewed outcome of a batch removal.
type Plan struct {
	Vertices []string     `json:"vertices"` // vertex IDs to remove, sorted
	Arcs     []string     `json:"arcs"`     // every removed arc ID, sorted
	Crossing []string     `json:"crossing"` // removed arcs with exactly one endpoint retained
	Pairs    []PairResult `json:"pairs"`
	Blocked  bool         `json:"blocked"`
	Scanned  int          `json:"scanned"`
}

// PlanPurge computes the removal closure of the given vertices and rules on
// each preserved pair against the fully merged after-state. It is pure: the
// head graph is never modified.
func PlanPurge(head *graph.Graph, adj *index.Adjacency, q Request) (Plan, error) {
	removed, err := removalSet(head, q.Vertices)
	if err != nil {
		return Plan{}, err
	}
	if len(q.Preserve) > MaxPurgePairs {
		return Plan{}, graph.Fail(graph.Invalid, "a purge protects at most %d pairs", MaxPurgePairs)
	}
	if q.Budget != 0 && (q.Budget < 1 || q.Budget > walk.MaxBudget) {
		return Plan{}, graph.Fail(graph.Invalid, "invalid expansion budget")
	}
	plan := Plan{
		Vertices: sortedKeys(removed),
		Arcs:     []string{},
		Crossing: []string{},
		Pairs:    make([]PairResult, len(q.Preserve)),
	}
	for _, id := range graph.Keys(head.Arcs) {
		arc := head.Arcs[id]
		fromGone, toGone := removed[arc.From], removed[arc.To]
		if fromGone || toGone {
			plan.Arcs = append(plan.Arcs, id)
			if fromGone != toGone {
				plan.Crossing = append(plan.Crossing, id)
			}
		}
	}
	for i, pair := range q.Preserve {
		plan.Pairs[i].Pair = pair
		if err := checkPair(head, removed, pair); err != nil {
			return Plan{}, err
		}
	}
	if err := plan.rulePairs(head, adj, removed, q.Budget); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

// ConfirmArcs verifies that the caller confirmed exactly the arc closure of
// this plan, self-loops and parallel arcs by their own IDs.
func (p Plan) ConfirmArcs(arcs []string) error {
	confirmed := slices.Clone(arcs)
	slices.Sort(confirmed)
	for i := 1; i < len(confirmed); i++ {
		if confirmed[i] == confirmed[i-1] {
			return graph.Fail(graph.Invalid, "arc %q is confirmed twice", confirmed[i])
		}
	}
	if !slices.Equal(confirmed, p.Arcs) {
		return graph.Fail(graph.Conflict, "confirmed arcs do not match the removal closure; re-preview")
	}
	return nil
}

// ApplyPurge stages an accepted plan on a private candidate. The head graph
// is left untouched.
func ApplyPurge(head *graph.Graph, plan Plan) (*graph.Graph, error) {
	if head.Revision == math.MaxUint64 {
		return nil, graph.Fail(graph.Budget, "revision space exhausted")
	}
	candidate := head.Clone()
	for _, id := range plan.Arcs {
		delete(candidate.Arcs, id)
	}
	for _, id := range plan.Vertices {
		delete(candidate.Vertices, id)
	}
	if err := catalog.Validate(candidate); err != nil {
		return nil, err
	}
	candidate.Revision++
	return candidate, nil
}

func removalSet(head *graph.Graph, ids []string) (map[string]bool, error) {
	if len(ids) == 0 || len(ids) > MaxPurgeVertices {
		return nil, graph.Fail(graph.Invalid, "a purge requires 1..%d vertices", MaxPurgeVertices)
	}
	removed := make(map[string]bool, len(ids))
	for _, id := range ids {
		if _, ok := head.Vertices[id]; !ok {
			return nil, graph.Fail(graph.Missing, "vertex %q is not in this revision", id)
		}
		if removed[id] {
			return nil, graph.Fail(graph.Invalid, "vertex %q is listed twice", id)
		}
		removed[id] = true
	}
	return removed, nil
}

func checkPair(head *graph.Graph, removed map[string]bool, pair Pair) error {
	for _, id := range []string{pair.From, pair.To} {
		if _, ok := head.Vertices[id]; !ok {
			return graph.Fail(graph.Missing, "preserved pair endpoint %q is not in this revision", id)
		}
		if removed[id] {
			return graph.Fail(graph.Invalid, "preserved pair endpoint %q is in the removal set", id)
		}
	}
	return nil
}

// groupKey shares one traversal between pairs that agree on root, direction
// and arc kinds.
type groupKey struct {
	from      string
	direction index.Direction
	kinds     string
}

func keyOf(pair Pair) groupKey {
	kinds := slices.Clone(pair.Kinds)
	slices.Sort(kinds)
	kinds = slices.Compact(kinds)
	direction := pair.Direction
	if direction == "" {
		direction = index.Out
	}
	return groupKey{pair.From, direction, strings.Join(kinds, "\x00")}
}

// rulePairs rules on every pair. Pairs whose current cheapest route avoids
// the removal set survive without further work; only pairs touching the
// removed region are re-checked against the merged after-state, which is
// built at most once.
func (p *Plan) rulePairs(head *graph.Graph, adj *index.Adjacency, removed map[string]bool, budget int) error {
	groups := make(map[groupKey][]int)
	for i, pair := range p.Pairs {
		key := keyOf(pair.Pair)
		groups[key] = append(groups[key], i)
	}
	var after *graph.Graph
	var afterAdj *index.Adjacency
	for _, key := range sortedGroupKeys(groups) {
		indexes := groups[key]
		targets := make([]string, 0, len(indexes))
		seen := make(map[string]bool, len(indexes))
		for _, i := range indexes {
			if to := p.Pairs[i].To; !seen[to] {
				seen[to] = true
				targets = append(targets, to)
			}
		}
		slices.Sort(targets)
		selection := walk.Selection{
			Kinds: strings.Split(key.kinds, "\x00"), Direction: key.direction, Budget: budget,
		}
		if key.kinds == "" {
			selection.Kinds = nil
		}
		before, err := walk.Routes(head, adj, walk.RouteRequest{From: key.from, Targets: targets, Selection: selection})
		if err != nil {
			return err
		}
		p.Scanned += before.Scanned
		var touched []string
		for _, target := range targets {
			route, ok := before.Routes[target]
			switch {
			case !ok:
				p.rule(indexes, target, PairResult{Status: Absent})
			case survives(route, removed):
				p.rule(indexes, target, PairResult{Status: Kept, Before: route.Arcs, After: route.Arcs})
			default:
				touched = append(touched, target)
			}
		}
		if len(touched) == 0 {
			continue
		}
		if after == nil {
			after = head.Clone()
			for _, id := range p.Arcs {
				delete(after.Arcs, id)
			}
			for _, id := range p.Vertices {
				delete(after.Vertices, id)
			}
			afterAdj = index.BuildAdjacency(after)
		}
		remaining, err := walk.Routes(after, afterAdj, walk.RouteRequest{From: key.from, Targets: touched, Selection: selection})
		if err != nil {
			return err
		}
		p.Scanned += remaining.Scanned
		for _, target := range touched {
			if route, ok := remaining.Routes[target]; ok {
				p.rule(indexes, target, PairResult{Status: Kept, Before: before.Routes[target].Arcs, After: route.Arcs})
			} else {
				p.rule(indexes, target, PairResult{Status: Broken, Before: before.Routes[target].Arcs})
				p.Blocked = true
			}
		}
	}
	return nil
}

// rule assigns the ruling to every input pair in the group aiming at target.
func (p *Plan) rule(indexes []int, target string, ruling PairResult) {
	for _, i := range indexes {
		if p.Pairs[i].To == target {
			p.Pairs[i].Status = ruling.Status
			p.Pairs[i].Before = ruling.Before
			p.Pairs[i].After = ruling.After
		}
	}
}

// survives reports whether a route avoids every removed vertex, which also
// avoids every removed arc: only arcs incident to removed vertices disappear.
func survives(route walk.Route, removed map[string]bool) bool {
	for _, vertex := range route.Vertices {
		if removed[vertex] {
			return false
		}
	}
	return true
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func sortedGroupKeys(groups map[groupKey][]int) []groupKey {
	keys := make([]groupKey, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b groupKey) int {
		if n := strings.Compare(a.from, b.from); n != 0 {
			return n
		}
		if n := strings.Compare(string(a.direction), string(b.direction)); n != 0 {
			return n
		}
		return strings.Compare(a.kinds, b.kinds)
	})
	return keys
}
