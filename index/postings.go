package index

import (
	"cmp"
	"slices"
	"sort"

	"rillweave/graph"
)

type postingKey struct {
	kind, property string
	value          graph.Scalar
}

type fieldKey struct {
	kind, property string
}

type entry struct {
	value graph.Scalar
	id    string
}

type Postings struct {
	values  map[postingKey][]string
	ordered map[fieldKey][]entry
}

func BuildPostings(g *graph.Graph) *Postings {
	p := &Postings{
		values:  make(map[postingKey][]string),
		ordered: make(map[fieldKey][]entry),
	}
	for _, id := range graph.Keys(g.Vertices) {
		v := g.Vertices[id]
		for property, value := range v.Properties {
			key := postingKey{v.Kind, property, value}
			p.values[key] = append(p.values[key], id)
			field := fieldKey{v.Kind, property}
			p.ordered[field] = append(p.ordered[field], entry{value, id})
		}
	}
	for field, entries := range p.ordered {
		slices.SortFunc(entries, compareEntry)
		p.ordered[field] = entries
	}
	return p
}

// compareEntry orders by the typed scalar value first, then by vertex ID. The
// ID tie-break keeps values that compare equal individually addressable, so a
// continuation token never skips a vertex that shares its value.
func compareEntry(a, b entry) int {
	if c := graph.Compare(a.value, b.value); c != 0 {
		return c
	}
	return cmp.Compare(a.id, b.id)
}

type Posting struct {
	IDs       []string `json:"ids"`
	Matched   int      `json:"matched"`
	Truncated bool     `json:"truncated"`
}

func (p *Postings) Equal(kind, property string, value graph.Scalar, limit int) Posting {
	ids := p.values[postingKey{kind, property, value}]
	return Posting{
		IDs:     append([]string{}, ids[:min(limit, len(ids))]...),
		Matched: len(ids), Truncated: len(ids) > limit,
	}
}

// Position is a total-order location inside one typed property: a scalar value
// and the vertex ID that breaks ties between equal values. It is the resume
// point carried by an ordered lookup continuation.
type Position struct {
	Value graph.Scalar
	ID    string
}

// Page is a bounded window of an ordered posting. Matched always counts the
// whole range, independent of the window and of the resume position.
type Page struct {
	IDs       []string
	Matched   int
	Truncated bool
	Last      *Position
}

// Scan reads the vertices of kind whose property falls inside r, in typed value
// order and then vertex ID order. Vertices that do not store the property have
// no entry and therefore never enter the range. after, when present, must be an
// existing entry inside r; the page resumes strictly after it.
func (p *Postings) Scan(kind, property string, r graph.Range, after *Position, limit int) (Page, error) {
	entries := p.ordered[fieldKey{kind, property}]
	// Filter bounds first, then compare against the resume position as a total
	// order. Equal scalars are distinguished by ID, so a page boundary inside a
	// run of equal values resumes exactly after the last emitted vertex.
	first := 0
	if r.From != nil {
		from := *r.From
		first = sort.Search(len(entries), func(i int) bool {
			return atLeast(entries[i].value, from)
		})
	}
	last := len(entries)
	if r.To != nil {
		to := *r.To
		last = sort.Search(len(entries), func(i int) bool {
			return past(entries[i].value, to)
		})
	}
	if last < first {
		last = first
	}
	start := first
	if after != nil {
		target := entry{after.Value, after.ID}
		start = sort.Search(len(entries), func(i int) bool {
			return compareEntry(entries[i], target) > 0
		})
		if start == 0 || compareEntry(entries[start-1], target) != 0 {
			return Page{}, graph.Fail(graph.Invalid, "continuation position does not match this property's order")
		}
		if start < first || start > last {
			return Page{}, graph.Fail(graph.Invalid, "continuation position is outside the requested range")
		}
	}
	page := Page{IDs: []string{}, Matched: last - first}
	end := min(start+limit, last)
	for _, item := range entries[start:end] {
		page.IDs = append(page.IDs, item.id)
	}
	page.Truncated = end < last
	if page.Truncated && end > start {
		next := entries[end-1]
		page.Last = &Position{Value: next.value, ID: next.id}
	}
	return page, nil
}

// atLeast reports whether value lies at or beyond an inclusive or exclusive
// lower bound.
func atLeast(value graph.Scalar, from graph.Bound) bool {
	c := graph.Compare(value, from.Value)
	return c > 0 || c == 0 && !from.Exclusive
}

// past reports whether value lies beyond an inclusive or exclusive upper bound.
func past(value graph.Scalar, to graph.Bound) bool {
	c := graph.Compare(value, to.Value)
	return c > 0 || c == 0 && to.Exclusive
}
