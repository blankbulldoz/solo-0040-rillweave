package index

import "rillweave/graph"

type Direction string

const (
	Out  Direction = "out"
	In   Direction = "in"
	Both Direction = "both"
)

type Adjacency struct {
	out    map[string][]graph.Arc
	in     map[string][]graph.Arc
	byKind map[string][]graph.Arc
}

func BuildAdjacency(g *graph.Graph) *Adjacency {
	a := &Adjacency{
		out:    make(map[string][]graph.Arc),
		in:     make(map[string][]graph.Arc),
		byKind: make(map[string][]graph.Arc),
	}
	for _, id := range graph.Keys(g.Arcs) {
		edge := g.Arcs[id]
		a.out[edge.From] = append(a.out[edge.From], edge)
		a.in[edge.To] = append(a.in[edge.To], edge)
		a.byKind[edge.Kind] = append(a.byKind[edge.Kind], edge)
	}
	return a
}

// ArcsOfKind lists every arc of the given kind in ascending arc ID order.
// Parallel arcs keep their individual identities.
func (a *Adjacency) ArcsOfKind(kind string) []graph.Arc {
	return a.byKind[kind]
}

// Visit enumerates candidate arcs by ID. A loop occupies the head of both
// postings at the same time and is visited once in a bidirectional read.
func (a *Adjacency) Visit(id string, d Direction, visit func(graph.Arc, string) error) error {
	var out, in []graph.Arc
	if d != In {
		out = a.out[id]
	}
	if d != Out {
		in = a.in[id]
	}
	for len(out) != 0 || len(in) != 0 {
		switch {
		case len(in) == 0 || len(out) != 0 && out[0].ID <= in[0].ID:
			arc := out[0]
			out = out[1:]
			if len(in) > 0 && arc.ID == in[0].ID {
				in = in[1:]
			}
			if err := visit(arc, arc.To); err != nil {
				return err
			}
		default:
			arc := in[0]
			in = in[1:]
			if err := visit(arc, arc.From); err != nil {
				return err
			}
		}
	}
	return nil
}
