package collate

import (
	"sort"

	"rillweave/graph"
)

// structuralCensus rejects differences that no renaming can repair: schema
// shape, vertex/arc counts per kind, and the multiset of exact vertex
// signatures (which already folds in degree, direction, parallel multiplicity,
// cost and neighbor colors). It returns census rows suitable for a report.
func structuralCensus(left, right *graph.Graph) (string, []CensusMismatch, []CensusMismatch) {
	if reason := sameSchema(left.Schema, right.Schema); reason != "" {
		return reason, nil, nil
	}
	if len(left.Vertices) != len(right.Vertices) {
		return "vertex counts differ", nil, nil
	}
	if len(left.Arcs) != len(right.Arcs) {
		return "arc counts differ", nil, nil
	}

	vertexMismatches := censusByKind(
		keysOf(left.Vertices, func(v graph.Vertex) string { return v.Kind }),
		keysOf(right.Vertices, func(v graph.Vertex) string { return v.Kind }),
	)
	if len(vertexMismatches) != 0 {
		return "vertex kind census differs", vertexMismatches, nil
	}
	arcMismatches := censusByKind(
		keysOf(left.Arcs, func(a graph.Arc) string { return a.Kind }),
		keysOf(right.Arcs, func(a graph.Arc) string { return a.Kind }),
	)
	if len(arcMismatches) != 0 {
		return "arc kind census differs", nil, arcMismatches
	}

	leftSigs := make(map[string]int, len(left.Vertices))
	rightSigs := make(map[string]int, len(right.Vertices))
	for id := range left.Vertices {
		leftSigs[sigKey(signature(left, id))]++
	}
	for id := range right.Vertices {
		rightSigs[sigKey(signature(right, id))]++
	}
	if !sameCensus(leftSigs, rightSigs) {
		return "vertex signature multisets differ (local degrees, directions, parallel arc counts, costs or neighbor colors cannot be aligned)", vertexMismatches, arcMismatches
	}
	return "", nil, nil
}

func keysOf[V any](m map[string]V, kind func(V) string) map[string]int {
	out := make(map[string]int, len(m))
	for _, v := range m {
		out[kind(v)]++
	}
	return out
}

func censusByKind(left, right map[string]int) []CensusMismatch {
	keys := make(map[string]bool, len(left)+len(right))
	for k := range left {
		keys[k] = true
	}
	for k := range right {
		keys[k] = true
	}
	out := []CensusMismatch{}
	for k := range keys {
		if left[k] != right[k] {
			out = append(out, CensusMismatch{Kind: k, LeftCount: left[k], RightCount: right[k]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

func sameCensus(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// sameSchema compares the catalog up to kind names. The graphs were imported
// independently, so names and revisions are expected to differ and are not
// part of equivalence; the rule structure must be identical.
func sameSchema(a, b graph.Schema) string {
	if len(a.Vertices) != len(b.Vertices) {
		return "schema vertex kind sets differ"
	}
	if len(a.Arcs) != len(b.Arcs) {
		return "schema arc kind sets differ"
	}
	for kind, rule := range a.Vertices {
		other, ok := b.Vertices[kind]
		if !ok {
			return "schema is missing vertex kind " + kind
		}
		if !samePropertyRules(rule.Properties, other.Properties) {
			return "schema property rules differ for vertex kind " + kind
		}
	}
	for kind, rule := range a.Arcs {
		other, ok := b.Arcs[kind]
		if !ok {
			return "schema is missing arc kind " + kind
		}
		if rule.From != other.From || rule.To != other.To {
			return "schema endpoint rules differ for arc kind " + kind
		}
		if !samePropertyRules(rule.Properties, other.Properties) {
			return "schema property rules differ for arc kind " + kind
		}
	}
	return ""
}

func samePropertyRules(a, b map[string]graph.PropertyRule) bool {
	if len(a) != len(b) {
		return false
	}
	for key, rule := range a {
		if b[key] != rule {
			return false
		}
	}
	return true
}
