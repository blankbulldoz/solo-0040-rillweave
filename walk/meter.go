package walk

import "rillweave/graph"

// meter accounts for candidate arc inspections against one request's
// allowance. A traversal charges every candidate before kind selection, so
// arcs rejected by selection and repeated inspections are billed exactly like
// arcs handed to a consumer.
type meter struct {
	allowance int
	scanned   int
}

// charge bills one candidate inspection. The boundary is inclusive: exactly
// allowance candidates may be inspected, and the next inspection fails the
// whole query rather than returning a partial result.
func (m *meter) charge() error {
	if m.scanned == m.allowance {
		return graph.Fail(graph.Budget, "expansion budget exhausted after %d candidate arcs", m.scanned)
	}
	m.scanned++
	return nil
}
