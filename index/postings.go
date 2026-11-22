package index

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"rillweave/graph"
)

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

// DigestIDs is the canonical short description of a match set: the lowercase
// hexadecimal SHA-256 of the sorted IDs joined by a single newline. The empty
// set digests the empty string.
func DigestIDs(ids []string) string {
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	return hex.EncodeToString(sum[:])
}

type Posting struct {
	IDs       []string `json:"ids"`
	Matched   int      `json:"matched"`
	Truncated bool     `json:"truncated"`
	Digest    string   `json:"digest"`
}

func (p *Postings) Equal(kind, property string, value graph.Scalar, limit int) Posting {
	ids := p.values[postingKey{kind, property, value}]
	return Posting{
		IDs:     append([]string{}, ids[:min(limit, len(ids))]...),
		Matched: len(ids), Truncated: len(ids) > limit,
		Digest: DigestIDs(ids),
	}
}

// Match returns a copy of the complete sorted match set for a typed equality.
func (p *Postings) Match(kind, property string, value graph.Scalar) []string {
	return append([]string{}, p.values[postingKey{kind, property, value}]...)
}
