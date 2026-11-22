package walk

import "slices"

// label pairs a candidate route with its accumulated cost. Exact-depth search
// keeps one label per vertex and depth; compare decides which of two labels at
// that depth survives and which one is reported.
type label struct {
	cost    int64
	witness witness
}

// prefer reports whether a should replace b when both are candidates for the
// same vertex at the same exact depth. Ties prefer fewer arcs, then the
// lexicographically smallest arc ID sequence.
func prefer(a, b label) bool {
	if a.cost != b.cost {
		return a.cost < b.cost
	}
	if a.witness.steps != b.witness.steps {
		return a.witness.steps < b.witness.steps
	}
	return slices.Compare(a.witness.arcs(), b.witness.arcs()) < 0
}

// frontier holds the surviving label for each vertex at one exact depth.
type frontier map[string]label

// admit offers a label to its vertex at the current depth, keeping the
// stable winner and reporting whether the offer displaced the prior label.
func (f frontier) admit(target string, candidate label) bool {
	prior, seen := f[target]
	if seen && !prefer(candidate, prior) {
		return false
	}
	f[target] = candidate
	return true
}
