package index

import "rillweave/graph"

type Direction string

const (
	Out  Direction = "out"
	In   Direction = "in"
	Both Direction = "both"
)

type Adjacency struct {
	out map[string][]graph.Arc
	in  map[string][]graph.Arc
}

func BuildAdjacency(g *graph.Graph) *Adjacency {
	a := &Adjacency{
		out: make(map[string][]graph.Arc),
		in:  make(map[string][]graph.Arc),
	}
	for _, id := range graph.Keys(g.Arcs) {
		edge := g.Arcs[id]
		a.out[edge.From] = append(a.out[edge.From], edge)
		a.in[edge.To] = append(a.in[edge.To], edge)
	}
	return a
}

// Visit enumerates candidate arcs by ID. A loop occupies the head of both
// postings at the same time and is visited once in a bidirectional read.
func (a *Adjacency) Visit(id string, d Direction, visit func(graph.Arc, string) error) error {
	_, _, err := a.VisitWindow(id, d, 0, 0, func(graph.Arc, string) (bool, error) {
		return false, nil
	}, visit)
	return err
}

// VisitWindow enumerates the same deterministic ordering as Visit but resumes
// at the given posting offsets and can stop early. A true stop result ends the
// window after the current candidate, leaving the returned offsets pointing at
// the next candidate to inspect. A loop is still visited once when the two
// postings merge.
func (a *Adjacency) VisitWindow(
	id string, d Direction, outOffset, inOffset int,
	stop func(graph.Arc, string) (bool, error),
	visit func(graph.Arc, string) error,
) (nextOut, nextIn int, err error) {
	var out, in []graph.Arc
	if d != In {
		out = a.out[id]
	}
	if d != Out {
		in = a.in[id]
	}
	i, j := outOffset, inOffset
	for i < len(out) || j < len(in) {
		switch {
		case j >= len(in) || i < len(out) && out[i].ID <= in[j].ID:
			arc := out[i]
			i++
			if j < len(in) && arc.ID == in[j].ID {
				j++
			}
			if err = visit(arc, arc.To); err != nil {
				return i, j, err
			}
			if halt, haltErr := stop(arc, arc.To); haltErr != nil {
				return i, j, haltErr
			} else if halt {
				return i, j, nil
			}
		default:
			arc := in[j]
			j++
			if i < len(out) && arc.ID == out[i].ID {
				i++
			}
			if err = visit(arc, arc.From); err != nil {
				return i, j, err
			}
			if halt, haltErr := stop(arc, arc.From); haltErr != nil {
				return i, j, haltErr
			} else if halt {
				return i, j, nil
			}
		}
	}
	return i, j, nil
}
