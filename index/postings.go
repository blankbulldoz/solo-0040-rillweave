package index

import "rillweave/graph"

type postingKey struct {
	kind, property string
	value          graph.Scalar
}

type Postings struct {
	values map[postingKey][]string
}

func BuildPostings(g *graph.Graph) *Postings {
	p := &Postings{values: make(map[postingKey][]string)}
	for _, id := range graph.Keys(g.Vertices) {
		v := g.Vertices[id]
		for property, value := range v.Properties {
			key := postingKey{v.Kind, property, value}
			p.values[key] = append(p.values[key], id)
		}
	}
	return p
}

type Posting struct {
	IDs       []string `json:"ids"`
	Matched   int      `json:"matched"`
	Truncated bool     `json:"truncated"`
}

func (p *Postings) Equal(kind, property string, value graph.Scalar, limit int) Posting {
	ids := p.values[postingKey{kind, property, value}]
	return Posting{
		IDs: append([]string{}, ids[:min(limit, len(ids))]...),
		Matched: len(ids), Truncated: len(ids) > limit,
	}
}

// Matches resolves the full typed posting without an output window. The
// returned slice is owned by the caller; missing properties simply never
// appear in a posting, so they cannot match a present value.
func (p *Postings) Matches(kind, property string, value graph.Scalar) []string {
	ids := p.values[postingKey{kind, property, value}]
	return append([]string{}, ids...)
}
