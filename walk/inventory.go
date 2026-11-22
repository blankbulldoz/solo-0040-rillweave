package walk

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"slices"

	"rillweave/graph"
)

// InventoryRequest audits registered vertex pairs for a missing directed arc.
// Pairs are formed only between vertices of the two endpoint kinds that carry
// the same property value; a value registered on one side only cannot pair.
type InventoryRequest struct {
	FromKind string `json:"from_kind"`
	ToKind   string `json:"to_kind"`
	Property string `json:"property"`
	ArcKind  string `json:"arc_kind"`
	// Value is optional and carries only the scalar kind used to validate the
	// property rule; the audit itself covers every shared property value.
	Value  graph.Scalar `json:"value,omitempty"`
	Limit  int          `json:"limit,omitempty"`
	Budget int          `json:"budget,omitempty"`
	Cursor string       `json:"cursor,omitempty"`
}

// Gap points back at the two original vertex records between which the
// required directed arc kind is absent.
type Gap struct {
	From graph.Vertex `json:"from"`
	To   graph.Vertex `json:"to"`
}

// Cursor is the resumable scan position: the revision it belongs to, the next
// shared-value cohort and the next (from,to) pair offset inside it. A nil
// cursor starts at the first pair of the current revision.
type Cursor struct {
	Rev   uint64 `json:"r"`
	Value int    `json:"v"`
	From  int    `json:"f"`
	To    int    `json:"t"`
}

// InventoryResult returns one bounded window of gaps and an explicit
// completion flag. An empty page is "complete" only when every cohort was
// scanned; a budget or limit stop keeps the cursor so unprocessed pairs are
// never misreported as absence of gaps.
type InventoryResult struct {
	Gaps      []Gap  `json:"gaps"`
	Matched   int    `json:"matched"`
	Scanned   int    `json:"scanned"`
	Truncated bool   `json:"truncated"`
	Complete  bool   `json:"complete"`
	Cursor    string `json:"cursor,omitempty"`
}

type cohort struct {
	value graph.Scalar
	from  []string
	to    []string
}

type pairKey struct{ from, to string }

// Inventory enumerates same-value (from_kind,to_kind) pairs and reports those
// lacking an arc of the requested kind in the forward direction. A reverse
// arc or an arc of another kind never satisfies the requirement.
func Inventory(g *graph.Graph, q InventoryRequest) (InventoryResult, error) {
	limit, err := Limit(q.Limit)
	if err != nil {
		return InventoryResult{}, err
	}
	budget := q.Budget
	if budget == 0 {
		budget = MaxBudget
	}
	if budget < 1 || budget > MaxBudget {
		return InventoryResult{}, graph.Fail(graph.Invalid, "budget must be in [1,%d]", MaxBudget)
	}
	if err := inventoryRules(g, q); err != nil {
		return InventoryResult{}, err
	}
	start, err := decodeCursor(q.Cursor)
	if err != nil {
		return InventoryResult{}, err
	}
	if start.Rev != 0 && start.Rev != g.Revision {
		return InventoryResult{}, graph.Fail(graph.Conflict,
			"cursor belongs to revision %d but the selected view is revision %d", start.Rev, g.Revision)
	}

	cohorts := buildCohorts(g, q)
	if err := checkCursor(start, cohorts); err != nil {
		return InventoryResult{}, err
	}
	covered := buildCoverage(g, q.ArcKind)

	result := InventoryResult{Gaps: []Gap{}}
	stamp := func(c Cursor) string {
		c.Rev = g.Revision
		return encodeCursor(c)
	}
	ci, fi, ti := start.Value, start.From, start.To
	for ci < len(cohorts) {
		group := cohorts[ci]
		for fi < len(group.from) {
			for ti < len(group.to) {
				if result.Scanned == budget {
					result.Truncated = true
					result.Cursor = stamp(Cursor{Value: ci, From: fi, To: ti})
					return result, nil
				}
				result.Scanned++
				result.Matched++
				if !covered[pairKey{group.from[fi], group.to[ti]}] {
					result.Gaps = append(result.Gaps, Gap{
						From: g.Vertices[group.from[fi]],
						To:   g.Vertices[group.to[ti]],
					})
					if len(result.Gaps) == limit {
						result.Truncated = true
						result.Cursor = stamp(nextPosition(ci, fi, ti, group))
						return result, nil
					}
				}
				ti++
			}
			fi++
			ti = 0
		}
		ci++
		fi, ti = 0, 0
	}
	result.Complete = true
	return result, nil
}

// nextPosition returns the first pair not inspected after the pair at
// (fi,ti), advancing to the next cohort when the current one is exhausted.
func nextPosition(ci, fi, ti int, group cohort) Cursor {
	ti++
	if ti == len(group.to) {
		fi++
		ti = 0
	}
	if fi == len(group.from) {
		return Cursor{Value: ci + 1}
	}
	return Cursor{Value: ci, From: fi, To: ti}
}

func checkCursor(c Cursor, cohorts []cohort) error {
	if c.Value > len(cohorts) || c.From < 0 || c.To < 0 {
		return graph.Fail(graph.Invalid, "cursor is outside the current revision")
	}
	if c.Value == len(cohorts) {
		if c.From != 0 || c.To != 0 {
			return graph.Fail(graph.Invalid, "cursor is outside the current revision")
		}
		return nil
	}
	group := cohorts[c.Value]
	if c.From >= len(group.from) || c.To >= len(group.to) {
		return graph.Fail(graph.Invalid, "cursor is outside the current revision")
	}
	return nil
}

func inventoryRules(g *graph.Graph, q InventoryRequest) error {
	for label, kind := range map[string]string{"from_kind": q.FromKind, "to_kind": q.ToKind} {
		if _, ok := g.Schema.Vertices[kind]; !ok {
			return graph.Fail(graph.Invalid, "unknown vertex kind %q for %q", kind, label)
		}
	}
	rule, ok := g.Schema.Arcs[q.ArcKind]
	if !ok {
		return graph.Fail(graph.Invalid, "unknown arc kind %q", q.ArcKind)
	}
	if rule.From != q.FromKind || rule.To != q.ToKind {
		return graph.Fail(graph.Invalid, "arc kind %q requires %q -> %q endpoints", q.ArcKind, rule.From, rule.To)
	}
	fromRule, ok := g.Schema.Vertices[q.FromKind].Properties[q.Property]
	if !ok {
		return graph.Fail(graph.Invalid, "unknown property %q on vertex kind %q", q.Property, q.FromKind)
	}
	toRule, ok := g.Schema.Vertices[q.ToKind].Properties[q.Property]
	if !ok {
		return graph.Fail(graph.Invalid, "unknown property %q on vertex kind %q", q.Property, q.ToKind)
	}
	if fromRule.Kind != toRule.Kind {
		return graph.Fail(graph.Invalid, "property %q has different scalar kinds on the endpoint kinds", q.Property)
	}
	// An omitted value audits every shared value; a present value must match
	// the declared scalar kind and never undergoes coercion.
	if q.Value.Kind != "" && (q.Value.Kind != fromRule.Kind || !q.Value.Valid()) {
		return graph.Fail(graph.Invalid, "property %q requires %s", q.Property, fromRule.Kind)
	}
	return nil
}

// buildCohorts groups postings by the values registered on both endpoint
// kinds. Cohorts and their members are deterministic; only values present on
// both sides produce pair candidates, so unregistered values never pair.
func buildCohorts(g *graph.Graph, q InventoryRequest) []cohort {
	fromIDs := make(map[graph.Scalar][]string)
	toIDs := make(map[graph.Scalar][]string)
	for _, id := range graph.Keys(g.Vertices) {
		v := g.Vertices[id]
		value, ok := v.Properties[q.Property]
		if !ok {
			// A missing property never claims a value and cannot pair.
			continue
		}
		// The two endpoint kinds may be identical, so each side must be
		// populated independently; a switch would only take the first case.
		if v.Kind == q.FromKind {
			fromIDs[value] = append(fromIDs[value], id)
		}
		if v.Kind == q.ToKind {
			toIDs[value] = append(toIDs[value], id)
		}
	}
	values := make([]graph.Scalar, 0, len(fromIDs))
	for value := range fromIDs {
		if len(toIDs[value]) != 0 {
			values = append(values, value)
		}
	}
	slices.SortFunc(values, compareScalar)
	cohorts := make([]cohort, 0, len(values))
	for _, value := range values {
		cohorts = append(cohorts, cohort{value: value, from: fromIDs[value], to: toIDs[value]})
	}
	return cohorts
}

func compareScalar(a, b graph.Scalar) int {
	if a.Kind != b.Kind {
		return cmp.Compare(a.Kind, b.Kind)
	}
	switch a.Kind {
	case graph.Text:
		return cmp.Compare(a.Text, b.Text)
	case graph.Int:
		return cmp.Compare(a.Int, b.Int)
	default:
		return cmp.Compare(boolRank(a.Bool), boolRank(b.Bool))
	}
}

func boolRank(v bool) int {
	if v {
		return 1
	}
	return 0
}

// buildCoverage records endpoint pairs joined by at least one arc of the
// requested kind. Parallel arcs collapse to one covered pair; direction and
// kind are part of the key semantics.
func buildCoverage(g *graph.Graph, arcKind string) map[pairKey]bool {
	covered := make(map[pairKey]bool)
	for _, arc := range g.Arcs {
		if arc.Kind == arcKind {
			covered[pairKey{arc.From, arc.To}] = true
		}
	}
	return covered
}

func decodeCursor(raw string) (Cursor, error) {
	if raw == "" {
		return Cursor{}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return Cursor{}, graph.Fail(graph.Invalid, "cursor is not encoded correctly")
	}
	var cursor Cursor
	if err := json.Unmarshal(decoded, &cursor); err != nil {
		return Cursor{}, graph.Fail(graph.Invalid, "cursor does not describe a scan position")
	}
	return cursor, nil
}

func encodeCursor(c Cursor) string {
	encoded, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(encoded)
}
