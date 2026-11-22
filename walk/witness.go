package walk

import "rillweave/graph"

// witness is the private record of one candidate route. It is an immutable
// chain of steps, so extending a prefix never aliases a sibling candidate and
// the stored arc order is exactly the traversal order. The chain is the only
// representation the search shares; arc IDs and vertices are reconstructed in
// that order only when a result is materialized.
type witness struct {
	prefix *witness
	arc    string // zero at the start vertex
	vertex string
	steps  int
}

func witnessAt(vertex string) witness {
	return witness{vertex: vertex}
}

func (w witness) extend(arc graph.Arc, target string) witness {
	return witness{prefix: &w, arc: arc.ID, vertex: target, steps: w.steps + 1}
}

// arcs returns the stored arc IDs in traversal order.
func (w witness) arcs() []string {
	arcs := make([]string, w.steps)
	for node := &w; node.steps != 0; node = node.prefix {
		arcs[node.steps-1] = node.arc
	}
	return arcs
}

// vertices returns the visited vertices, starting with the source vertex.
func (w witness) vertices() []string {
	vertices := make([]string, w.steps+1)
	for node := &w; ; node = node.prefix {
		vertices[node.steps] = node.vertex
		if node.steps == 0 {
			return vertices
		}
	}
}
