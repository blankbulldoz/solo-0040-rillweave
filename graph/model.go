package graph

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"maps"
	"slices"
)

const (
	MaxVertices = 10000
	MaxArcs     = 40000
	MaxCost     = 1000000000
	MaxRules    = 64
	MaxProps    = 32
)

type PropertyRule struct {
	Kind     Kind `json:"kind"`
	Required bool `json:"required,omitempty"`
	Unique   bool `json:"unique,omitempty"`
}

type VertexRule struct {
	Properties map[string]PropertyRule `json:"properties,omitempty"`
}

type ArcRule struct {
	From       string                  `json:"from"`
	To         string                  `json:"to"`
	Properties map[string]PropertyRule `json:"properties,omitempty"`
}

type Schema struct {
	Vertices map[string]VertexRule `json:"vertices"`
	Arcs     map[string]ArcRule    `json:"arcs"`
}

type Vertex struct {
	ID         string            `json:"id"`
	Kind       string            `json:"kind"`
	Properties map[string]Scalar `json:"properties,omitempty"`
}

type Arc struct {
	ID         string            `json:"id"`
	Kind       string            `json:"kind"`
	From       string            `json:"from"`
	To         string            `json:"to"`
	Cost       int64             `json:"cost"`
	Properties map[string]Scalar `json:"properties,omitempty"`
}

type Graph struct {
	Name     string            `json:"name"`
	Identity string            `json:"identity"`
	Revision uint64            `json:"revision"`
	Schema   Schema            `json:"schema"`
	Vertices map[string]Vertex `json:"vertices"`
	Arcs     map[string]Arc    `json:"arcs"`
}

// NewIdentity mints a durable identity for a graph being installed. Creation
// and restore each receive a fresh identity; a commit or a restart keeps it.
func NewIdentity() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", Fail(Unavailable, "cannot allocate graph identity")
	}
	return hex.EncodeToString(raw[:]), nil
}

func New(name string, schema Schema) *Graph {
	return &Graph{
		Name: name, Revision: 1, Schema: schema,
		Vertices: make(map[string]Vertex), Arcs: make(map[string]Arc),
	}
}

// Clone copies the entity maps, not their values. Published property maps and
// rules are immutable; edits replace whole records.
func (g *Graph) Clone() *Graph {
	next := *g
	next.Vertices = maps.Clone(g.Vertices)
	next.Arcs = maps.Clone(g.Arcs)
	return &next
}

func Keys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func ValidID(id string) bool {
	if len(id) == 0 || len(id) > 64 || !alnum(id[0]) {
		return false
	}
	for i := 1; i < len(id); i++ {
		c := id[i]
		if !alnum(c) && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

func alnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
}

func LessIdentity(kindA, idA, kindB, idB string) int {
	if n := cmp.Compare(kindA, kindB); n != 0 {
		return n
	}
	return cmp.Compare(idA, idB)
}
