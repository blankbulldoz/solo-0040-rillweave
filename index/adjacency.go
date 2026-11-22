package index

import (
	"sort"

	"rillweave/graph"
)

type Direction string

const (
	Out  Direction = "out"
	In   Direction = "in"
	Both Direction = "both"
)

// kindTally counts incidence for one vertex and one arc kind. A loop occupies
// one row in each directional posting, so out and in are both incremented for
// it while loop records the overlap that a bidirectional read must subtract.
type kindTally struct {
	out  int
	in   int
	loop int
}

type Adjacency struct {
	out   map[string][]graph.Arc
	in    map[string][]graph.Arc
	tally map[string]map[string]kindTally
}

func BuildAdjacency(g *graph.Graph) *Adjacency {
	a := &Adjacency{
		out:   make(map[string][]graph.Arc),
		in:    make(map[string][]graph.Arc),
		tally: make(map[string]map[string]kindTally),
	}
	for _, id := range graph.Keys(g.Arcs) {
		edge := g.Arcs[id]
		a.out[edge.From] = append(a.out[edge.From], edge)
		a.in[edge.To] = append(a.in[edge.To], edge)

		from := a.tally[edge.From]
		if from == nil {
			from = make(map[string]kindTally)
			a.tally[edge.From] = from
		}
		outValue := from[edge.Kind]
		outValue.out++
		from[edge.Kind] = outValue

		to := a.tally[edge.To]
		if to == nil {
			to = make(map[string]kindTally)
			a.tally[edge.To] = to
		}
		inValue := to[edge.Kind]
		inValue.in++
		to[edge.Kind] = inValue

		if edge.From == edge.To {
			// The loop row overlaps the two postings of its single vertex;
			// record that overlap once so a bidirectional count subtracts one
			// row per loop, not two.
			loopValue := from[edge.Kind]
			loopValue.loop++
			from[edge.Kind] = loopValue
		}
	}
	return a
}

// Count returns the number of matching arcs for a vertex without inspecting
// the posting rows: a bidirectional read counts a loop once. An empty kind
// selection means every kind.
func (a *Adjacency) Count(vertex string, d Direction, kinds map[string]bool) int {
	tallies := a.tally[vertex]
	if tallies == nil {
		return 0
	}
	sum := func(t kindTally) int {
		switch d {
		case In:
			return t.in
		case Both:
			// Each loop is one arc present in both postings; t.loop is the
			// number of overlapping loop rows, so subtract that many rows,
			// not the (per-kind single) increment counted for each loop.
			return t.out + t.in - t.loop
		default:
			return t.out
		}
	}
	if len(kinds) == 0 {
		total := 0
		for _, t := range tallies {
			total += sum(t)
		}
		return total
	}
	total := 0
	for kind := range kinds {
		if t, ok := tallies[kind]; ok {
			total += sum(t)
		}
	}
	return total
}

// arcIterator merges the directional postings of one vertex in arc ID order.
// A loop occupies the head of both postings and is emitted once in a
// bidirectional read. Seek positions let a paginated read resume strictly
// after a previously returned arc ID without re-inspecting earlier rows.
type arcIterator struct {
	out, in []graph.Arc
	oi, ii  int
}

// Iterator returns an iterator positioned at the first arc whose ID is
// strictly greater than after in the selected direction.
func (a *Adjacency) Iterator(vertex string, d Direction, after string) *arcIterator {
	it := &arcIterator{}
	if d != In {
		rows := a.out[vertex]
		it.out = rows
		it.oi = sort.Search(len(rows), func(i int) bool { return rows[i].ID > after })
	}
	if d != Out {
		rows := a.in[vertex]
		it.in = rows
		it.ii = sort.Search(len(rows), func(i int) bool { return rows[i].ID > after })
	}
	return it
}

// Next returns the next arc, the neighbor vertex in traversal direction, and
// whether an arc was available.
func (it *arcIterator) Next() (graph.Arc, string, bool) {
	hasOut, hasIn := it.oi < len(it.out), it.ii < len(it.in)
	switch {
	case !hasOut && !hasIn:
		return graph.Arc{}, "", false
	case !hasIn:
		arc := it.out[it.oi]
		it.oi++
		return arc, arc.To, true
	case !hasOut:
		arc := it.in[it.ii]
		it.ii++
		return arc, arc.From, true
	case it.out[it.oi].ID <= it.in[it.ii].ID:
		arc := it.out[it.oi]
		it.oi++
		if arc.ID == it.in[it.ii].ID {
			it.ii++ // the same loop row is the head of both postings
		}
		return arc, arc.To, true
	default:
		arc := it.in[it.ii]
		it.ii++
		return arc, arc.From, true
	}
}

// Visit enumerates candidate arcs by ID from the start of the postings. A
// loop occupies the head of both postings at the same time and is visited
// once in a bidirectional read.
func (a *Adjacency) Visit(id string, d Direction, visit func(graph.Arc, string) error) error {
	it := a.Iterator(id, d, "")
	for {
		arc, next, ok := it.Next()
		if !ok {
			return nil
		}
		if err := visit(arc, next); err != nil {
			return err
		}
	}
}
