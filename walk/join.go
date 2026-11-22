package walk

import (
	"cmp"
	"slices"
	"strconv"
	"strings"

	"rillweave/catalog"
	"rillweave/graph"
	"rillweave/index"
)

// JoinSide names one side of a composite-key relation. Property names may
// differ between the sides; positions are paired in the given order.
type JoinSide struct {
	Kind       string   `json:"kind"`
	Properties []string `json:"properties"`
}

// JoinRequest relates two vertex kinds by a tuple of typed properties. Limit
// and Offset page the relation groups; EvidenceLimit and EvidenceOffset page
// individual pairs in one stable order.
type JoinRequest struct {
	Left           JoinSide `json:"left"`
	Right          JoinSide `json:"right"`
	Limit          int      `json:"limit,omitempty"`
	Offset         int      `json:"offset,omitempty"`
	EvidenceLimit  int      `json:"evidence_limit,omitempty"`
	EvidenceOffset int64    `json:"evidence_offset,omitempty"`
}

// JoinPlanEcho reports the validated key pairing that produced a result.
type JoinPlanEcho struct {
	LeftKind        string       `json:"left_kind"`
	RightKind       string       `json:"right_kind"`
	LeftProperties  []string     `json:"left_properties"`
	RightProperties []string     `json:"right_properties"`
	Types           []graph.Kind `json:"types"`
}

// JoinGroup describes one relation key present on both sides. Both counts are
// exact even when the group is not included on the requested page.
type JoinGroup struct {
	Key        []graph.Scalar `json:"key"`
	LeftCount  int            `json:"left_count"`
	RightCount int            `json:"right_count"`
	Pairs      int64          `json:"pairs"`
	Ambiguous  bool           `json:"ambiguous"`
}

// JoinEvidence is one related pair. Pairs are ordered by key tuple, then left
// vertex ID, then right vertex ID.
type JoinEvidence struct {
	Key   []graph.Scalar `json:"key"`
	Left  string         `json:"left"`
	Right string         `json:"right"`
}

type JoinResult struct {
	Plan            JoinPlanEcho   `json:"plan"`
	Groups          []JoinGroup    `json:"groups"`
	MatchedGroups   int            `json:"matched_groups"`
	Offset          int            `json:"offset"`
	Truncated       bool           `json:"truncated"`
	Pairs           int64          `json:"pairs"`
	AmbiguousGroups int            `json:"ambiguous_groups"`
	LeftVertices    int            `json:"left_vertices"`
	LeftKeyed       int            `json:"left_keyed"`
	LeftExcluded    int            `json:"left_excluded"`
	RightVertices   int            `json:"right_vertices"`
	RightKeyed      int            `json:"right_keyed"`
	RightExcluded   int            `json:"right_excluded"`
	LeftOnlyKeys    int            `json:"left_only_keys"`
	RightOnlyKeys   int            `json:"right_only_keys"`
	Evidence        []JoinEvidence `json:"evidence"`
	EvidenceTotal   int64          `json:"evidence_total"`
	EvidenceNext    int64          `json:"evidence_next"`
	Scanned         int            `json:"scanned"`
}

type sideEntry struct {
	key []graph.Scalar
	ids []string
}

type joinGroup struct {
	key         []graph.Scalar
	left, right []string
}

// Join groups two vertex kinds by a composite typed key. A vertex participates
// only when every key property is present, and values match exactly: there is
// no coercion, rounding or prefix rule. Group sizes and pair totals are exact;
// evidence is paged arithmetically, so a hot key's full pairing is never built.
func Join(g *graph.Graph, p *index.Postings, q JoinRequest) (JoinResult, error) {
	plan, err := catalog.JoinPlan(g.Schema, q.Left.Kind, q.Right.Kind, q.Left.Properties, q.Right.Properties)
	if err != nil {
		return JoinResult{}, err
	}
	groupLimit, err := Limit(q.Limit)
	if err != nil {
		return JoinResult{}, err
	}
	if q.Offset < 0 {
		return JoinResult{}, graph.Fail(graph.Invalid, "offset must be non-negative")
	}
	if q.EvidenceOffset < 0 || q.EvidenceLimit < 0 || q.EvidenceLimit > 1000 {
		return JoinResult{}, graph.Fail(graph.Invalid, "evidence window needs a non-negative offset and limit in [0,1000]")
	}
	if q.EvidenceOffset > 0 && q.EvidenceLimit == 0 {
		return JoinResult{}, graph.Fail(graph.Invalid, "evidence_offset requires a positive evidence_limit")
	}

	result := JoinResult{
		Plan: JoinPlanEcho{
			LeftKind:        plan.LeftKind,
			RightKind:       plan.RightKind,
			LeftProperties:  append([]string{}, plan.LeftProps...),
			RightProperties: append([]string{}, plan.RightProps...),
			Types:           append([]graph.Kind{}, plan.Kinds...),
		},
		Groups:   []JoinGroup{},
		Evidence: []JoinEvidence{},
		Offset:   q.Offset,
	}

	left, leftTotal, leftKeyed, leftExcluded := groupSide(g, p, plan.LeftKind, plan.LeftProps, plan.Kinds)
	right, rightTotal, rightKeyed, rightExcluded := groupSide(g, p, plan.RightKind, plan.RightProps, plan.Kinds)
	result.LeftVertices, result.LeftKeyed, result.LeftExcluded = leftTotal, leftKeyed, leftExcluded
	result.RightVertices, result.RightKeyed, result.RightExcluded = rightTotal, rightKeyed, rightExcluded
	result.Scanned = leftTotal + rightTotal

	shared := make([]joinGroup, 0, min(len(left), len(right)))
	for encoded, entry := range left {
		other, ok := right[encoded]
		if !ok {
			result.LeftOnlyKeys++
			continue
		}
		group := joinGroup{key: entry.key, left: entry.ids, right: other.ids}
		shared = append(shared, group)
		result.Pairs += int64(len(group.left)) * int64(len(group.right))
		if len(group.left) > 1 && len(group.right) > 1 {
			result.AmbiguousGroups++
		}
	}
	result.RightOnlyKeys = len(right) - (len(left) - result.LeftOnlyKeys)
	slices.SortFunc(shared, func(a, b joinGroup) int { return compareKey(a.key, b.key) })
	result.MatchedGroups = len(shared)
	result.EvidenceTotal = result.Pairs

	start := min(q.Offset, len(shared))
	end := min(start+groupLimit, len(shared))
	for _, group := range shared[start:end] {
		result.Groups = append(result.Groups, JoinGroup{
			Key:        append([]graph.Scalar{}, group.key...),
			LeftCount:  len(group.left),
			RightCount: len(group.right),
			Pairs:      int64(len(group.left)) * int64(len(group.right)),
			Ambiguous:  len(group.left) > 1 && len(group.right) > 1,
		})
	}
	result.Truncated = end < len(shared)

	if q.EvidenceLimit > 0 {
		result.Evidence, result.EvidenceNext = evidence(shared, q.EvidenceOffset, q.EvidenceLimit, result.Pairs)
	}
	return result, nil
}

// evidence materialises one window of the full pairing without walking the
// pairs that precede it. Each group contributes left_count*right_count rows in
// the global order, so a position maps to a group, then to a pair inside it.
func evidence(groups []joinGroup, offset int64, limit int, total int64) ([]JoinEvidence, int64) {
	rows := make([]JoinEvidence, 0, limit)
	pos := min(offset, total)
	base := int64(0)
	for _, group := range groups {
		count := int64(len(group.left)) * int64(len(group.right))
		finish := base + count
		if pos < finish {
			for local := pos - base; local < count && len(rows) < limit; local++ {
				rightCount := int64(len(group.right))
				rows = append(rows, JoinEvidence{
					Key:   group.key,
					Left:  group.left[local/rightCount],
					Right: group.right[local%rightCount],
				})
				pos++
			}
		}
		base = finish
		if len(rows) == limit {
			break
		}
	}
	return rows, pos
}

// groupSide buckets the vertices of one kind by their composite key. Vertices
// missing any component are excluded rather than treated as absent values, and
// a component of the wrong declared kind cannot occur in a validated graph.
func groupSide(g *graph.Graph, p *index.Postings, kind string, props []string, kinds []graph.Kind) (
	map[string]*sideEntry, int, int, int) {
	ids := p.KindIDs(kind)
	groups := make(map[string]*sideEntry, len(ids))
	keyed, excluded := 0, 0
	for _, id := range ids {
		components, ok := tuple(g.Vertices[id], props, kinds)
		if !ok {
			excluded++
			continue
		}
		keyed++
		encoded := encodeKey(components)
		entry := groups[encoded]
		if entry == nil {
			entry = &sideEntry{key: components}
			groups[encoded] = entry
		}
		entry.ids = append(entry.ids, id)
	}
	return groups, len(ids), keyed, excluded
}

// tuple reads every key property or reports that the vertex cannot join.
func tuple(v graph.Vertex, props []string, kinds []graph.Kind) ([]graph.Scalar, bool) {
	components := make([]graph.Scalar, len(props))
	for i, property := range props {
		value, ok := v.Properties[property]
		if !ok || value.Kind != kinds[i] {
			return nil, false
		}
		components[i] = value
	}
	return components, true
}

// encodeKey builds an unambiguous map key: a kind tag plus a length prefix for
// text, so distinct tuples never collide and no value is ever truncated.
func encodeKey(components []graph.Scalar) string {
	var b strings.Builder
	for _, value := range components {
		switch value.Kind {
		case graph.Text:
			b.WriteByte('t')
			b.WriteString(strconv.Itoa(len(value.Text)))
			b.WriteByte(':')
			b.WriteString(value.Text)
		case graph.Int:
			b.WriteByte('i')
			b.WriteString(strconv.FormatInt(value.Int, 10))
			b.WriteByte(';')
		default:
			b.WriteByte('b')
			if value.Bool {
				b.WriteByte('1')
			} else {
				b.WriteByte('0')
			}
			b.WriteByte(';')
		}
	}
	return b.String()
}

func compareKey(a, b []graph.Scalar) int {
	for i := range a {
		if n := compareScalar(a[i], b[i]); n != 0 {
			return n
		}
	}
	return 0
}

func compareScalar(a, b graph.Scalar) int {
	if n := cmp.Compare(a.Kind, b.Kind); n != 0 {
		return n
	}
	switch a.Kind {
	case graph.Text:
		return cmp.Compare(a.Text, b.Text)
	case graph.Int:
		return cmp.Compare(a.Int, b.Int)
	case graph.Bool:
		switch {
		case a.Bool == b.Bool:
			return 0
		case !a.Bool:
			return -1
		default:
			return 1
		}
	default:
		return 0
	}
}
