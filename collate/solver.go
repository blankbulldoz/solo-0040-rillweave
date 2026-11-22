package collate

import (
	"fmt"
	"maps"
	"sort"

	"rillweave/graph"
)

// Anchor fixes one correspondence between a left (reference) vertex and a
// right (imported) vertex. Anchors are constraints on the isomorphism, not
// evidence of equivalence by themselves.
type Anchor struct {
	Left  string
	Right string
}

// Budget bounds the search. Zero uses DefaultBudget.
type Budget struct {
	// Assignments caps complete vertex assignment attempts.
	Assignments int
}

const DefaultBudget = 1_000_000

type status string

const (
	StatusEquivalent   status = "equivalent"
	StatusInequivalent status = "inequivalent"
	StatusUndetermined status = "undetermined"
)

// Mapping is a reviewable renaming witness: every left entity paired with its
// right counterpart.
type Mapping struct {
	Vertices map[string]string `json:"vertices"`
	Arcs     map[string]string `json:"arcs"`
}

// CensusMismatch explains a failure at the invariant level, independent of any
// particular mapping choice.
type CensusMismatch struct {
	Kind       string `json:"kind"`
	LeftCount  int    `json:"left_count"`
	RightCount int    `json:"right_count"`
}

// ArcMismatch names an arc in the left graph whose color/direction cannot be
// placed under the attempted vertex correspondence.
type ArcMismatch struct {
	Arc    string `json:"arc"`
	Kind   string `json:"kind"`
	From   string `json:"from"`
	To     string `json:"to"`
	Cost   int64  `json:"cost"`
	Reason string `json:"reason"`
}

// Result is the fixed dual-view verdict.
type Result struct {
	Status        status           `json:"status"`
	Reason        string           `json:"reason,omitempty"`
	Mapping       *Mapping         `json:"mapping,omitempty"`
	VertexCensus  []CensusMismatch `json:"vertex_census,omitempty"`
	ArcCensus     []CensusMismatch `json:"arc_census,omitempty"`
	ArcMismatches []ArcMismatch    `json:"arc_mismatches,omitempty"`
	AnchorsUsed   int              `json:"anchors_used"`
	Assignments   int              `json:"assignments_tried"`
	Exhausted     bool             `json:"budget_exhausted,omitempty"`
}

type solver struct {
	left, right *graph.Graph
	sigL, sigR  map[string]Signature

	// byColor groups right vertices sharing an exact signature.
	byColor map[string][]string

	// mapL: left id -> right id; mapR: right id -> left id.
	mapL map[string]string
	mapR map[string]string

	// Remaining right vertices per exact-signature class, as multisets of ids.
	pool map[string]map[string]bool

	budget    int
	tried     int
	exhausted bool

	// failedLeaf retains the deepest complete correspondence that failed the
	// arc-row check, so the reported evidence names real mapped endpoints
	// instead of the empty map left after backtracking unwinds.
	failedLeaf map[string]string
}

// Equivalent searches for an isomorphism between left and right that respects
// the anchors. An undetermined result means the search budget was spent inside
// an ambiguous (typically highly symmetric) graph, never that a single failed
// mapping proves inequivalence.
func Equivalent(left, right *graph.Graph, anchors []Anchor, limit Budget) Result {
	if limit.Assignments < 0 {
		return Result{Status: StatusInequivalent, Reason: "negative search budget"}
	}
	budget := limit.Assignments
	if budget == 0 {
		budget = DefaultBudget
	}

	base := Result{AnchorsUsed: len(anchors)}
	if reason, mismatches, arcMismatches := structuralCensus(left, right); reason != "" {
		base.Status = StatusInequivalent
		base.Reason = reason
		base.VertexCensus = mismatches
		base.ArcCensus = arcMismatches
		return base
	}

	s := newSolver(left, right, budget)
	if err := s.fixAnchors(anchors); err != nil {
		base.Status = StatusInequivalent
		base.Reason = err.Error()
		return base
	}

	order := s.assignmentOrder()
	if !s.backtrack(order, 0) {
		if s.exhausted {
			base.Status = StatusUndetermined
			base.Reason = fmt.Sprintf("search budget of %d assignment attempts exhausted before the remaining symmetries were resolved", budget)
			base.Exhausted = true
			base.Assignments = s.tried
			return base
		}
		base.Status = StatusInequivalent
		base.Reason = "no vertex correspondence preserves kinds, properties, arc direction, parallel multiplicity and cost"
		base.ArcMismatches = s.collectArcEvidence()
		base.Assignments = s.tried
		return base
	}

	mapping := s.buildMapping()
	base.Status = StatusEquivalent
	base.Reason = "an anchor-respecting isomorphism preserves every vertex and arc row"
	base.Mapping = &mapping
	base.Assignments = s.tried
	return base
}

func newSolver(left, right *graph.Graph, budget int) *solver {
	s := &solver{
		left: left, right: right, budget: budget,
		sigL: make(map[string]Signature), sigR: make(map[string]Signature),
		byColor: make(map[string][]string),
		mapL:    make(map[string]string), mapR: make(map[string]string),
		pool: make(map[string]map[string]bool),
	}
	for id := range left.Vertices {
		s.sigL[id] = signature(left, id)
	}
	for id := range right.Vertices {
		sig := signature(right, id)
		s.sigR[id] = sig
		key := sigKey(sig)
		s.byColor[key] = append(s.byColor[key], id)
		if s.pool[key] == nil {
			s.pool[key] = make(map[string]bool)
		}
		s.pool[key][id] = true
	}
	for key := range s.byColor {
		sort.Strings(s.byColor[key])
	}
	return s
}

func (s *solver) fixAnchors(anchors []Anchor) error {
	seen := make(map[string]bool, len(anchors))
	for _, anchor := range anchors {
		if _, ok := s.left.Vertices[anchor.Left]; !ok {
			return fmt.Errorf("anchor references unknown left vertex %q", anchor.Left)
		}
		if _, ok := s.right.Vertices[anchor.Right]; !ok {
			return fmt.Errorf("anchor references unknown right vertex %q", anchor.Right)
		}
		if sigKey(s.sigL[anchor.Left]) != sigKey(s.sigR[anchor.Right]) {
			return fmt.Errorf("anchor %q -> %q joins vertices with different kinds, properties or local incidence", anchor.Left, anchor.Right)
		}
		if prior, taken := s.mapL[anchor.Left]; taken && prior != anchor.Right {
			return fmt.Errorf("left vertex %q is anchored to both %q and %q", anchor.Left, prior, anchor.Right)
		}
		if prior, taken := s.mapR[anchor.Right]; taken && prior != anchor.Left {
			return fmt.Errorf("right vertex %q is anchored to both %q and %q", anchor.Right, prior, anchor.Left)
		}
		if seen[anchor.Left] {
			continue
		}
		seen[anchor.Left] = true
		s.mapL[anchor.Left] = anchor.Right
		s.mapR[anchor.Right] = anchor.Left
		delete(s.pool[sigKey(s.sigR[anchor.Right])], anchor.Right)
	}
	return nil
}

// assignmentOrder places anchored vertices first (they need no choice), then
// unanchored vertices by ascending candidate pool size, the most constrained
// first (minimum remaining values heuristic).
func (s *solver) assignmentOrder() []string {
	remaining := make([]string, 0, len(s.left.Vertices))
	for id := range s.left.Vertices {
		if _, anchored := s.mapL[id]; !anchored {
			remaining = append(remaining, id)
		}
	}
	sort.Slice(remaining, func(i, j int) bool {
		pi := len(s.pool[sigKey(s.sigL[remaining[i]])])
		pj := len(s.pool[sigKey(s.sigL[remaining[j]])])
		if pi != pj {
			return pi < pj
		}
		return remaining[i] < remaining[j]
	})
	anchored := make([]string, 0, len(s.mapL))
	for id := range s.mapL {
		anchored = append(anchored, id)
	}
	sort.Strings(anchored)
	return append(anchored, remaining...)
}

func (s *solver) backtrack(order []string, depth int) bool {
	if s.tried >= s.budget {
		s.exhausted = true
		return false
	}
	if depth == len(order) {
		if s.tried >= s.budget {
			s.exhausted = true
			return false
		}
		s.tried++
		if s.fullArcCheck() {
			return true
		}
		s.failedLeaf = maps.Clone(s.mapL)
		return false
	}
	leftID := order[depth]
	if rightID, anchored := s.mapL[leftID]; anchored {
		if s.localConsistent(leftID, rightID) {
			return s.backtrack(order, depth+1)
		}
		return false
	}
	key := sigKey(s.sigL[leftID])
	candidates := make([]string, 0, len(s.pool[key]))
	for id := range s.pool[key] {
		candidates = append(candidates, id)
	}
	sort.Strings(candidates)
	for _, rightID := range candidates {
		if !s.localConsistent(leftID, rightID) {
			continue
		}
		s.assign(leftID, rightID, key)
		if s.backtrack(order, depth+1) {
			return true
		}
		s.unassign(leftID, rightID, key)
		if s.exhausted {
			return false
		}
	}
	return false
}

func (s *solver) assign(leftID, rightID, key string) {
	s.mapL[leftID] = rightID
	s.mapR[rightID] = leftID
	delete(s.pool[key], rightID)
}

func (s *solver) unassign(leftID, rightID, key string) {
	delete(s.mapL, leftID)
	delete(s.mapR, rightID)
	s.pool[key][rightID] = true
}

// localConsistent prunes a candidate using only already-mapped neighbors:
// every incident arc whose other endpoint is mapped must have a matching
// right arc row (color, direction and mapped endpoints) with remaining
// multiplicity. This rejects a wrong branch long before the leaf.
func (s *solver) localConsistent(leftID, rightID string) bool {
	need := make(map[arcRowKey]int)
	for _, arcID := range graph.Keys(s.left.Arcs) {
		a := s.left.Arcs[arcID]
		if a.From != leftID && a.To != leftID {
			continue
		}
		mappedFrom, fromOK := s.mapL[a.From]
		mappedTo, toOK := s.mapL[a.To]
		if !fromOK || !toOK {
			continue
		}
		if (a.From == leftID && mappedFrom != rightID) || (a.To == leftID && mappedTo != rightID) {
			return false
		}
		need[arcRowKey{color: colorArc(a), from: mappedFrom, to: mappedTo}]++
	}
	if len(need) == 0 {
		return true
	}
	have := make(map[arcRowKey]int)
	for _, arcID := range graph.Keys(s.right.Arcs) {
		a := s.right.Arcs[arcID]
		if a.From != rightID && a.To != rightID {
			continue
		}
		if _, fromOK := s.mapR[a.From]; !fromOK {
			continue
		}
		if _, toOK := s.mapR[a.To]; !toOK {
			continue
		}
		have[arcRowKey{color: colorArc(a), from: a.From, to: a.To}]++
	}
	for row, count := range need {
		if have[row] < count {
			return false
		}
	}
	return true
}

// fullArcCheck verifies the complete arc multiset once all vertices are
// mapped: identical rows (arc color plus mapped endpoints) must appear with
// identical multiplicity on both sides.
func (s *solver) fullArcCheck() bool {
	if len(s.left.Arcs) != len(s.right.Arcs) {
		return false
	}
	rows := make(map[arcRowKey]int, len(s.left.Arcs))
	for _, a := range s.left.Arcs {
		from, to := s.mapL[a.From], s.mapL[a.To]
		if from == "" || to == "" {
			return false
		}
		rows[arcRowKey{color: colorArc(a), from: from, to: to}]++
	}
	for _, a := range s.right.Arcs {
		row := arcRowKey{color: colorArc(a), from: a.From, to: a.To}
		if rows[row] == 0 {
			return false
		}
		rows[row]--
	}
	for _, remaining := range rows {
		if remaining != 0 {
			return false
		}
	}
	return true
}

// collectArcEvidence re-derives concrete, reviewable mismatches using the
// deepest complete correspondence the search reached.
func (s *solver) collectArcEvidence() []ArcMismatch {
	if len(s.failedLeaf) == 0 {
		return []ArcMismatch{{Reason: "no complete vertex correspondence was reachable from the admissible signature classes"}}
	}
	out := []ArcMismatch{}
	rightRows := make(map[arcRowKey]int, len(s.right.Arcs))
	for _, a := range s.right.Arcs {
		rightRows[arcRowKey{color: colorArc(a), from: a.From, to: a.To}]++
	}
	for _, id := range graph.Keys(s.left.Arcs) {
		a := s.left.Arcs[id]
		from, to := s.failedLeaf[a.From], s.failedLeaf[a.To]
		reason := ""
		switch {
		case from == "" || to == "":
			reason = "endpoint could not be matched to any right vertex"
		default:
			row := arcRowKey{color: colorArc(a), from: from, to: to}
			if rightRows[row] == 0 {
				reason = fmt.Sprintf("no right arc of kind %q with cost %d runs from %q to %q", a.Kind, a.Cost, from, to)
			} else {
				rightRows[row]--
				continue
			}
		}
		out = append(out, ArcMismatch{Arc: id, Kind: a.Kind, From: a.From, To: a.To, Cost: a.Cost, Reason: reason})
	}
	if len(out) == 0 {
		out = append(out, ArcMismatch{Reason: "global arc multiplicities differ despite matching vertex colors"})
	}
	return out
}

func (s *solver) buildMapping() Mapping {
	m := Mapping{Vertices: make(map[string]string, len(s.mapL)), Arcs: make(map[string]string)}
	for l, r := range s.mapL {
		m.Vertices[l] = r
	}
	// Match arc identities greedily within identical mapped rows so the arc
	// mapping itself is reviewable; parallel arcs are paired one-to-one.
	type pending struct {
		leftID  string
		rightID []string
	}
	groups := make(map[arcRowKey]*pending)
	for id, a := range s.right.Arcs {
		row := arcRowKey{color: colorArc(a), from: a.From, to: a.To}
		if groups[row] == nil {
			groups[row] = &pending{}
		}
		groups[row].rightID = append(groups[row].rightID, id)
	}
	for _, p := range groups {
		sort.Strings(p.rightID)
	}
	for _, id := range graph.Keys(s.left.Arcs) {
		a := s.left.Arcs[id]
		row := arcRowKey{color: colorArc(a), from: s.mapL[a.From], to: s.mapL[a.To]}
		p := groups[row]
		if p != nil && len(p.rightID) != 0 {
			m.Arcs[id] = p.rightID[0]
			p.rightID = p.rightID[1:]
		}
	}
	return m
}

// arcRowKey identifies an arc row up to identity renaming.
type arcRowKey struct {
	color ArcColor
	from  string
	to    string
}

func sigKey(sig Signature) string {
	out := "v|" + sig.Color.Kind + "|" + sig.Color.Properties
	for _, in := range sig.Incidence {
		dir := "o"
		if in.Direction == In {
			dir = "i"
		}
		out += "\n" + dir + "|" + in.Arc.Kind + "|" + fmt.Sprintf("%d", in.Arc.Cost) + "|" +
			in.Arc.Properties + "|" + in.Other.Kind + "|" + in.Other.Properties
	}
	return out
}
