package catalog

import "rillweave/graph"

// MaxJoinKey bounds the number of properties in one composite key. The bound
// keeps a key tuple small enough to order and to echo in a response.
const MaxJoinKey = 16

// JoinKey is a validated composite-key plan for relating two vertex kinds. The
// two sides may use different property names, but position i of each side must
// declare the same scalar kind, so the tuples compare without coercion.
type JoinKey struct {
	LeftKind   string
	RightKind  string
	LeftProps  []string
	RightProps []string
	Kinds      []graph.Kind
}

// JoinPlan validates a composite-key relation against an installed schema. It
// rejects unknown kinds or properties, repeated properties within one side and
// positions whose declared kinds differ.
func JoinPlan(s graph.Schema, leftKind, rightKind string, leftProps, rightProps []string) (JoinKey, error) {
	leftRule, ok := s.Vertices[leftKind]
	if !ok {
		return JoinKey{}, graph.Fail(graph.Invalid, "unknown left vertex kind %q", leftKind)
	}
	rightRule, ok := s.Vertices[rightKind]
	if !ok {
		return JoinKey{}, graph.Fail(graph.Invalid, "unknown right vertex kind %q", rightKind)
	}
	if len(leftProps) == 0 || len(leftProps) > MaxJoinKey || len(leftProps) != len(rightProps) {
		return JoinKey{}, graph.Fail(graph.Invalid,
			"join requires 1..%d key properties with equal arity on both sides", MaxJoinKey)
	}
	plan := JoinKey{
		LeftKind: leftKind, RightKind: rightKind,
		LeftProps:  append([]string{}, leftProps...),
		RightProps: append([]string{}, rightProps...),
		Kinds:      make([]graph.Kind, len(leftProps)),
	}
	leftSeen := make(map[string]bool, len(leftProps))
	rightSeen := make(map[string]bool, len(rightProps))
	for i := range leftProps {
		lp, rp := leftProps[i], rightProps[i]
		if !graph.ValidID(lp) || !graph.ValidID(rp) {
			return JoinKey{}, graph.Fail(graph.Invalid, "invalid join property name at position %d", i)
		}
		if leftSeen[lp] {
			return JoinKey{}, graph.Fail(graph.Invalid, "left join property %q is repeated", lp)
		}
		if rightSeen[rp] {
			return JoinKey{}, graph.Fail(graph.Invalid, "right join property %q is repeated", rp)
		}
		leftSeen[lp], rightSeen[rp] = true, true
		lrule, ok := leftRule.Properties[lp]
		if !ok {
			return JoinKey{}, graph.Fail(graph.Invalid, "unknown property %q on left kind %q", lp, leftKind)
		}
		rrule, ok := rightRule.Properties[rp]
		if !ok {
			return JoinKey{}, graph.Fail(graph.Invalid, "unknown property %q on right kind %q", rp, rightKind)
		}
		if lrule.Kind != rrule.Kind {
			return JoinKey{}, graph.Fail(graph.Invalid,
				"key position %d pairs %q (%s) with %q (%s), which are not the same type",
				i, lp, lrule.Kind, rp, rrule.Kind)
		}
		plan.Kinds[i] = lrule.Kind
	}
	return plan, nil
}
