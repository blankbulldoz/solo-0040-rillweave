package index

import "rillweave/graph"

type postingKey struct {
	kind, property string
	value          graph.Scalar
}

type Postings struct {
	values map[postingKey][]string
	kinds  map[string][]string
}

func BuildPostings(g *graph.Graph) *Postings {
	p := &Postings{
		values: make(map[postingKey][]string),
		kinds:  make(map[string][]string),
	}
	for _, id := range graph.Keys(g.Vertices) {
		v := g.Vertices[id]
		p.kinds[v.Kind] = append(p.kinds[v.Kind], id)
		for property, value := range v.Properties {
			key := postingKey{v.Kind, property, value}
			p.values[key] = append(p.values[key], id)
		}
	}
	return p
}

// KindIDs lists every vertex of a kind in stable ID order. A vertex can appear
// here while contributing to no posting at all, because an absent property is
// not a value.
func (p *Postings) KindIDs(kind string) []string {
	return p.kinds[kind]
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
