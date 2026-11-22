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

// Cursor enumerates incidence candidates for one vertex without consuming them
// all at once. It preserves the merge order of Visit so callers that stop early
// still observe the same arc sequence as a complete read.
type Cursor struct {
	out []graph.Arc
	in  []graph.Arc
}

// Cursor returns a resumable iterator over the same candidates Visit yields.
// A loop occupies the head of both postings at the same time and appears once
// in a bidirectional cursor.
func (a *Adjacency) Cursor(id string, d Direction) *Cursor {
	c := &Cursor{}
	if d != In {
		c.out = a.out[id]
	}
	if d != Out {
		c.in = a.in[id]
	}
	return c
}

// Pending reports whether any candidate remains without consuming it. Callers
// can distinguish an exhausted vertex from an exhausted work allowance.
func (c *Cursor) Pending() bool {
	return len(c.out) != 0 || len(c.in) != 0
}

// Next returns the next candidate arc and the endpoint reached by leaving id
// through it. It reports false once the incidence stream is exhausted.
func (c *Cursor) Next() (graph.Arc, string, bool) {
	for len(c.out) != 0 || len(c.in) != 0 {
		switch {
		case len(c.in) == 0 || len(c.out) != 0 && c.out[0].ID <= c.in[0].ID:
			arc := c.out[0]
			c.out = c.out[1:]
			if len(c.in) > 0 && arc.ID == c.in[0].ID {
				c.in = c.in[1:]
			}
			return arc, arc.To, true
		default:
			arc := c.in[0]
			c.in = c.in[1:]
			return arc, arc.From, true
		}
	}
	return graph.Arc{}, "", false
}

// Visit enumerates candidate arcs by ID. A loop occupies the head of both
// postings at the same time and is visited once in a bidirectional read.
func (a *Adjacency) Visit(id string, d Direction, visit func(graph.Arc, string) error) error {
	c := a.Cursor(id, d)
	for {
		arc, next, ok := c.Next()
		if !ok {
			return nil
		}
		if err := visit(arc, next); err != nil {
			return err
		}
	}
}
