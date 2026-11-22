package graph

import "cmp"

// Compare orders two scalars of the same kind. Values of different kinds are
// ordered by kind name so callers that compare schema-validated bounds still
// get a deterministic result.
func Compare(a, b Scalar) int {
	if a.Kind != b.Kind {
		return cmp.Compare(string(a.Kind), string(b.Kind))
	}
	switch a.Kind {
	case Int:
		return cmp.Compare(a.Int, b.Int)
	case Text:
		return cmp.Compare(a.Text, b.Text)
	case Bool:
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

// Bound is one endpoint of a typed range. Exclusive excludes values equal to
// Value; the default is inclusive.
type Bound struct {
	Value     Scalar `json:"value"`
	Exclusive bool   `json:"exclusive,omitempty"`
}

// Range selects the values between two optional typed bounds. An absent bound
// is open-ended, so an empty Range covers every vertex that stores the
// property and has a schema-supported ordered kind.
type Range struct {
	From *Bound `json:"from,omitempty"`
	To   *Bound `json:"to,omitempty"`
}
