package walk

import (
	"cmp"
	"slices"

	"rillweave/graph"
	"rillweave/index"
)

// Exact scheduling is a precedence-constrained bin-packing search whose search
// space grows exponentially with the number of condensation groups. Larger
// closures are served by the bounded heuristic with explicit bounds instead.
const (
	exactGroupLimit  = 24
	exactSearchLimit = 200000
	MaxParallel      = 1000
)

type StagesRequest struct {
	Targets     []string `json:"targets"`
	MaxParallel int      `json:"max_parallel"`
	Selection
}

type StageGroup struct {
	ID       string   `json:"id"`
	Vertices []string `json:"vertices"`
}

type Stage struct {
	Stage  int          `json:"stage"`
	Groups []StageGroup `json:"groups"`
}

type BoundaryArc struct {
	Arc    string `json:"arc"`
	Kind   string `json:"kind"`
	From   string `json:"from"`
	To     string `json:"to"`
	Source string `json:"source_group"`
	Target string `json:"target_group"`
}

type WaitReason struct {
	Group          string   `json:"group"`
	Stage          int      `json:"stage"`
	EarliestStage  int      `json:"earliest_stage"`
	WaitingOn      []string `json:"waiting_on"`
	BoundaryArcs   []string `json:"boundary_arcs"`
	CapacityHeldBy []string `json:"capacity_held_by,omitempty"`
}

type StagesResult struct {
	Targets      []string      `json:"targets"`
	Included     []string      `json:"included"`
	Groups       []Component   `json:"groups"`
	BoundaryArcs []BoundaryArc `json:"boundary_arcs"`
	Stages       []Stage       `json:"stages"`
	WaitReasons  []WaitReason  `json:"wait_reasons"`
	LowerBound   int           `json:"lower_bound_stages"`
	UpperBound   int           `json:"upper_bound_stages"`
	Optimal      bool          `json:"optimal"`
	Method       string        `json:"method"`
	Scanned      int           `json:"scanned"`
}

// Stages turns the selected subgraph's strong components into a parallel
// execution plan for the closure of the requested targets. Every SCC is one
// indivisible scheduling unit, so vertices on a cycle always share a stage.
func Stages(g *graph.Graph, a *index.Adjacency, q StagesRequest) (StagesResult, error) {
	if len(q.Targets) == 0 {
		return StagesResult{}, graph.Fail(graph.Invalid, "targets must contain at least one vertex")
	}
	if len(q.Targets) > MaxParallel {
		return StagesResult{}, graph.Fail(graph.Invalid, "target count must not exceed %d", MaxParallel)
	}
	if q.MaxParallel < 1 || q.MaxParallel > MaxParallel {
		return StagesResult{}, graph.Fail(graph.Invalid, "max_parallel must be in [1,%d]", MaxParallel)
	}
	seenTarget := make(map[string]bool, len(q.Targets))
	for _, id := range q.Targets {
		if !graph.ValidID(id) {
			return StagesResult{}, graph.Fail(graph.Invalid, "target vertex id %q is invalid", id)
		}
		if seenTarget[id] {
			return StagesResult{}, graph.Fail(graph.Invalid, "target vertex %q is listed more than once", id)
		}
		seenTarget[id] = true
		if err := requireVertex(g, id); err != nil {
			return StagesResult{}, err
		}
	}
	t, err := topologyFor(g, a, q.Selection)
	if err != nil {
		return StagesResult{}, err
	}

	// Pull in every prerequisite: the closure follows reverse dependency arcs,
	// so a group is included only if it can reach a requested target.
	closure := make(map[string]bool, len(q.Targets))
	for _, target := range q.Targets {
		if err := reachBack(t, target, closure); err != nil {
			return StagesResult{}, err
		}
	}

	// Partition the closure with the same SCC semantics as Components, but
	// without paying for vertices that cannot affect the requested targets.
	ids := make([]string, 0, len(closure))
	for id := range closure {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	order := make([]string, 0, len(ids))
	finished := make(map[string]bool, len(ids))
	for _, id := range ids {
		if err := finishIn(t, id, closure, finished, &order); err != nil {
			return StagesResult{}, err
		}
	}
	components := []Component{}
	membership := make(map[string]string, len(ids))
	clear(finished)
	for i := len(order) - 1; i >= 0; i-- {
		if finished[order[i]] {
			continue
		}
		var members []string
		if err := collectIn(t, order[i], closure, finished, &members); err != nil {
			return StagesResult{}, err
		}
		slices.Sort(members)
		for _, member := range members {
			membership[member] = members[0]
		}
		components = append(components, Component{ID: members[0], Vertices: members})
	}
	slices.SortFunc(components, func(x, y Component) int { return cmp.Compare(x.ID, y.ID) })

	// Build the condensation DAG restricted to the closure. Cross-group arcs
	// keep their original identities so every ordering decision is auditable.
	pred := make(map[string]map[string]bool, len(components))
	succ := make(map[string]map[string]bool, len(components))
	boundary := []BoundaryArc{}
	boundaryByPair := make(map[string][]string)
	for _, c := range components {
		pred[c.ID] = map[string]bool{}
		succ[c.ID] = map[string]bool{}
	}
	for _, id := range ids {
		err := t.visit(id, index.Out, func(arc graph.Arc, next string) error {
			from, to := membership[id], membership[next]
			if from == to || from == "" || to == "" {
				return nil
			}
			boundary = append(boundary, BoundaryArc{
				Arc: arc.ID, Kind: arc.Kind, From: arc.From, To: arc.To,
				Source: from, Target: to,
			})
			key := from + "\x00" + to
			boundaryByPair[key] = append(boundaryByPair[key], arc.ID)
			pred[to][from] = true
			succ[from][to] = true
			return nil
		})
		if err != nil {
			return StagesResult{}, err
		}
	}
	slices.SortFunc(boundary, func(x, y BoundaryArc) int { return cmp.Compare(x.Arc, y.Arc) })
	for key := range boundaryByPair {
		slices.Sort(boundaryByPair[key])
	}

	groupIDs := make([]string, len(components))
	for i, c := range components {
		groupIDs[i] = c.ID
	}
	dag := dagModel{
		ids:   groupIDs,
		pred:  pred,
		succ:  succ,
		level: longestPaths(groupIDs, pred),
	}
	levelLower := 0
	for _, level := range dag.level {
		if level > levelLower {
			levelLower = level
		}
	}
	widthLower := (len(groupIDs) + q.MaxParallel - 1) / q.MaxParallel
	lowerBound := max(levelLower, widthLower)

	var assignment map[string]int
	optimal := false
	method := "exact"
	if len(groupIDs) <= exactGroupLimit {
		if plan, ok := scheduleExact(dag, q.MaxParallel); ok {
			assignment = plan
			optimal = true
		} else {
			assignment = scheduleGreedy(dag, q.MaxParallel)
			method = "heuristic"
		}
	} else {
		assignment = scheduleGreedy(dag, q.MaxParallel)
		method = "heuristic"
	}

	stageCount := 0
	for _, stage := range assignment {
		if stage > stageCount {
			stageCount = stage
		}
	}
	stages := make([]Stage, stageCount)
	for s := range stages {
		stages[s] = Stage{Stage: s + 1, Groups: []StageGroup{}}
	}
	membersOf := make(map[string]Component, len(components))
	for _, c := range components {
		membersOf[c.ID] = c
	}
	for _, id := range groupIDs {
		s := assignment[id] - 1
		stages[s].Groups = append(stages[s].Groups, StageGroup{ID: id, Vertices: membersOf[id].Vertices})
	}
	for s := range stages {
		slices.SortFunc(stages[s].Groups, func(x, y StageGroup) int { return cmp.Compare(x.ID, y.ID) })
	}

	reasons := buildWaitReasons(groupIDs, assignment, dag, boundaryByPair, q.MaxParallel)

	return StagesResult{
		Targets:      slices.Clone(q.Targets),
		Included:     slices.Clone(ids),
		Groups:       components,
		BoundaryArcs: boundary,
		Stages:       stages,
		WaitReasons:  reasons,
		LowerBound:   lowerBound,
		UpperBound:   stageCount,
		Optimal:      optimal,
		Method:       method,
		Scanned:      t.scanned,
	}, nil
}

type dagModel struct {
	ids   []string
	pred  map[string]map[string]bool
	succ  map[string]map[string]bool
	level map[string]int
}

// longestPaths returns the one-based longest predecessor chain ending at each
// group; it is the minimum stage each group could occupy on unlimited lanes.
func longestPaths(ids []string, links map[string]map[string]bool) map[string]int {
	level := make(map[string]int, len(ids))
	var visit func(string) int
	visit = func(id string) int {
		if n, ok := level[id]; ok {
			return n
		}
		best := 1
		for p := range links[id] {
			if n := visit(p) + 1; n > best {
				best = n
			}
		}
		level[id] = best
		return best
	}
	for _, id := range ids {
		visit(id)
	}
	return level
}

// scheduleExact searches for the minimum number of bounded stages. It uses
// iterative deepening over the stage budget: each feasibility pass answers
// "can the remaining groups finish within k more stages?" with a definite
// result, so every memoized answer is exact and plan extraction is a second
// pass over the same proven table.
func scheduleExact(dag dagModel, width int) (map[string]int, bool) {
	n := len(dag.ids)
	index := make(map[string]int, n)
	for i, id := range dag.ids {
		index[id] = i
	}
	predMask := make([]uint32, n)
	succMask := make([]uint32, n)
	for i, id := range dag.ids {
		for p := range dag.pred[id] {
			predMask[i] |= 1 << index[p]
		}
		for s := range dag.succ[id] {
			succMask[i] |= 1 << index[s]
		}
	}
	all := uint32(1<<n - 1)
	readyOf := func(done uint32) uint32 {
		ready := uint32(0)
		for i := 0; i < n; i++ {
			bit := uint32(1) << i
			if done&bit == 0 && predMask[i]&done == predMask[i] {
				ready |= bit
			}
		}
		return ready
	}
	// feasible reports whether the groups not in done can be placed in at most
	// budget more stages. memo caches definite answers and the first feasible
	// next-stage option per (state, remaining budget), so no separate replay is
	// needed.
	type memoEntry struct {
		ok     bool
		option uint32
	}
	type stateKey struct {
		done   uint32
		budget int
	}
	visited := 0
	exhausted := false
	var feasible func(done uint32, budget int, memo map[stateKey]memoEntry) memoEntry
	feasible = func(done uint32, budget int, memo map[stateKey]memoEntry) memoEntry {
		if done == all {
			return memoEntry{ok: true}
		}
		if budget == 0 {
			return memoEntry{ok: false}
		}
		// Width lower bound: not enough stages left to hold the groups.
		if remaining := n - popcount(done); (remaining+width-1)/width > budget {
			return memoEntry{ok: false}
		}
		key := stateKey{done, budget}
		if entry, seen := memo[key]; seen {
			return entry
		}
		visited++
		if visited > exactSearchLimit {
			exhausted = true
			return memoEntry{ok: false}
		}
		ready := readyOf(done)
		remaining := all ^ done
		result := memoEntry{ok: false}
		forEachSubset(ready, width, func(option uint32) bool {
			if exhausted {
				return true
			}
			// Skip under-filled choices when an idle ready group gates no
			// pending work: that group can join this stage for free, so the
			// fuller choice dominates this one without changing feasibility.
			if popcount(option) < width && hasUngatedIdle(ready^option, remaining^option, succMask) {
				return false
			}
			child := feasible(done|option, budget-1, memo)
			if child.ok {
				result = memoEntry{ok: true, option: option}
				return true
			}
			return false
		})
		if !exhausted {
			memo[key] = result
		}
		return result
	}

	// Lower bound on the optimum: longest predecessor chain and packing width.
	lower := 0
	for _, l := range dag.level {
		if l > lower {
			lower = l
		}
	}
	if packed := (n + width - 1) / width; packed > lower {
		lower = packed
	}
	upper := 0
	for _, stage := range scheduleGreedy(dag, width) {
		if stage > upper {
			upper = stage
		}
	}
	var proven map[stateKey]memoEntry
	optimum := 0
	for budget := lower; budget <= upper; budget++ {
		visited = 0
		exhausted = false
		memo := map[stateKey]memoEntry{}
		if root := feasible(0, budget, memo); root.ok {
			proven = memo
			optimum = budget
			break
		}
		if exhausted {
			return nil, false
		}
	}
	if optimum == 0 {
		return nil, false
	}

	// Extract the plan by following the proven option at each state while the
	// remaining stage budget decreases.
	assignment := make(map[string]int, n)
	done := uint32(0)
	remaining := optimum
	for stage := 1; done != all; stage++ {
		if stage > n {
			return nil, false
		}
		entry, ok := proven[stateKey{done, remaining}]
		if !ok || !entry.ok {
			return nil, false
		}
		option := entry.option
		for i := 0; i < n; i++ {
			if option&(1<<i) != 0 {
				assignment[dag.ids[i]] = stage
			}
		}
		done |= option
		remaining--
	}
	return assignment, true
}

// scheduleGreedy is the bounded fallback: among precedence-ready groups it
// schedules the deepest remaining chain first, which minimises the risk of
// leaving a long dependency path waiting behind short independent work.
func scheduleGreedy(dag dagModel, width int) map[string]int {
	done := map[string]bool{}
	assignment := map[string]int{}
	remainingDepth := longestPaths(dag.ids, dag.succ)
	for stage := 1; len(done) < len(dag.ids); stage++ {
		ready := []string{}
		for _, id := range dag.ids {
			if done[id] {
				continue
			}
			blocked := false
			for p := range dag.pred[id] {
				if !done[p] {
					blocked = true
					break
				}
			}
			if !blocked {
				ready = append(ready, id)
			}
		}
		slices.SortFunc(ready, func(x, y string) int {
			if n := cmp.Compare(remainingDepth[y], remainingDepth[x]); n != 0 {
				return n
			}
			return cmp.Compare(x, y)
		})
		if len(ready) > width {
			ready = ready[:width]
		}
		for _, id := range ready {
			assignment[id] = stage
			done[id] = true
		}
	}
	return assignment
}

func buildWaitReasons(ids []string, assignment map[string]int, dag dagModel,
	boundaryByPair map[string][]string, width int) []WaitReason {
	occupants := map[int][]string{}
	for _, id := range ids {
		stage := assignment[id]
		occupants[stage] = append(occupants[stage], id)
	}
	reasons := []WaitReason{}
	for _, id := range ids {
		stage := assignment[id]
		waitingOn := []string{}
		arcs := []string{}
		for p := range dag.pred[id] {
			waitingOn = append(waitingOn, p)
			key := p + "\x00" + id
			arcs = append(arcs, boundaryByPair[key]...)
		}
		slices.Sort(waitingOn)
		slices.Sort(arcs)
		earliest := 1
		for _, p := range waitingOn {
			if assignment[p]+1 > earliest {
				earliest = assignment[p] + 1
			}
		}
		reason := WaitReason{
			Group: id, Stage: stage, EarliestStage: earliest,
			WaitingOn: waitingOn, BoundaryArcs: arcs,
		}
		if stage > earliest && len(occupants[earliest]) >= width {
			// Every lane at the earliest legal stage was occupied; name the
			// groups that held the capacity so the deferral is explainable.
			heldBy := slices.Clone(occupants[earliest])
			slices.Sort(heldBy)
			reason.CapacityHeldBy = heldBy
		}
		reasons = append(reasons, reason)
	}
	return reasons
}

func reachBack(t *topology, root string, reached map[string]bool) error {
	if reached[root] {
		return nil
	}
	reached[root] = true
	return t.visit(root, index.In, func(_ graph.Arc, next string) error {
		return reachBack(t, next, reached)
	})
}

func finishIn(t *topology, id string, scope, seen map[string]bool, order *[]string) error {
	if seen[id] || !scope[id] {
		return nil
	}
	seen[id] = true
	err := t.visit(id, index.Out, func(_ graph.Arc, next string) error {
		if scope[next] {
			return finishIn(t, next, scope, seen, order)
		}
		return nil
	})
	if err == nil {
		*order = append(*order, id)
	}
	return err
}

func collectIn(t *topology, id string, scope, seen map[string]bool, members *[]string) error {
	if seen[id] || !scope[id] {
		return nil
	}
	seen[id] = true
	*members = append(*members, id)
	return t.visit(id, index.In, func(_ graph.Arc, next string) error {
		if scope[next] {
			return collectIn(t, next, scope, seen, members)
		}
		return nil
	})
}

// forEachSubset visits nonempty subsets of bits sized at most limit. The
// enumeration is size-first and deterministic; stop returning true ends it.
func forEachSubset(bits uint32, limit int, visit func(uint32) bool) {
	positions := []uint32{}
	for i := uint32(0); i < 32; i++ {
		if bits&(1<<i) != 0 {
			positions = append(positions, i)
		}
	}
	var enumerate func(start, chosen uint32, count int) bool
	enumerate = func(start, chosen uint32, count int) bool {
		if count > 0 {
			if visit(chosen) {
				return true
			}
		}
		if count == limit {
			return false
		}
		for i := int(start); i < len(positions); i++ {
			if enumerate(uint32(i+1), chosen|(1<<positions[i]), count+1) {
				return true
			}
		}
		return false
	}
	enumerate(0, 0, 0)
}

func popcount(v uint32) int {
	count := 0
	for v != 0 {
		v &= v - 1
		count++
	}
	return count
}

// hasUngatedIdle reports whether at least one idle ready group has no
// successor among the groups that remain after the current choice. Such a
// group can be added to the choice for free, so an under-filled choice leaving
// it idle is never part of an optimal plan.
func hasUngatedIdle(idle, pending uint32, succMask []uint32) bool {
	for i := 0; i < len(succMask); i++ {
		if idle&(1<<i) != 0 && succMask[i]&pending == 0 {
			return true
		}
	}
	return false
}
