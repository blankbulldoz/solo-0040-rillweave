// Package collate decides whether two imported graph images describe the same
// logical graph when vertex and arc identities may have been renamed. Image
// digests only certify byte equality, so equivalence here is a structural
// isomorphism test over kinds, properties, arc direction, parallel multiplicity
// and cost. A caller may fix part of the vertex correspondence through anchors;
// the remaining correspondence is searched rather than guessed.
package collate

import (
	"cmp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"rillweave/graph"
)

// VertexColor is everything about a vertex that must survive renaming: its
// kind and its complete, order-independent property assignment.
type VertexColor struct {
	Kind       string
	Properties string
}

// ArcColor is the renaming-invariant identity of one arc. Parallel arcs are
// distinct rows; the backtracking layer matches them by multiplicity.
type ArcColor struct {
	Kind       string
	Cost       int64
	Properties string
}

// Incidence summarizes one endpoint role of a vertex: one row per incident arc
// of a given color in a given direction, with the color of the other endpoint.
// Self-loops occupy both directions, matching the adjacency contract.
type Incidence struct {
	Arc       ArcColor
	Direction Direction
	Other     VertexColor
}

type Direction uint8

const (
	Out Direction = iota
	In
)

func colorVertex(v graph.Vertex) VertexColor {
	return VertexColor{Kind: v.Kind, Properties: canonicalProperties(v.Properties)}
}

func colorArc(a graph.Arc) ArcColor {
	return ArcColor{Kind: a.Kind, Cost: a.Cost, Properties: canonicalProperties(a.Properties)}
}

// Signature is the complete local invariant of a vertex. Two vertices can only
// correspond when their signatures are exactly equal.
type Signature struct {
	Color     VertexColor
	Incidence []Incidence
}

func signature(g *graph.Graph, id string) Signature {
	s := Signature{Color: colorVertex(g.Vertices[id]), Incidence: []Incidence{}}
	for _, arcID := range graph.Keys(g.Arcs) {
		a := g.Arcs[arcID]
		arc := colorArc(a)
		if a.From == id {
			s.Incidence = append(s.Incidence, Incidence{Arc: arc, Direction: Out, Other: colorVertex(g.Vertices[a.To])})
		}
		if a.To == id {
			s.Incidence = append(s.Incidence, Incidence{Arc: arc, Direction: In, Other: colorVertex(g.Vertices[a.From])})
		}
	}
	sort.Slice(s.Incidence, func(i, j int) bool { return lessIncidence(s.Incidence[i], s.Incidence[j]) })
	return s
}

func lessIncidence(a, b Incidence) bool {
	if a.Direction != b.Direction {
		return a.Direction < b.Direction
	}
	if c := cmpArcColor(a.Arc, b.Arc); c != 0 {
		return c < 0
	}
	return cmpVertexColor(a.Other, b.Other) < 0
}

func cmpVertexColor(a, b VertexColor) int {
	if c := cmp.Compare(a.Kind, b.Kind); c != 0 {
		return c
	}
	return cmp.Compare(a.Properties, b.Properties)
}

func cmpArcColor(a, b ArcColor) int {
	if c := cmp.Compare(a.Kind, b.Kind); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Cost, b.Cost); c != 0 {
		return c
	}
	return cmp.Compare(a.Properties, b.Properties)
}

// canonicalProperties renders a property map deterministically. Scalar values
// carry their own type, so the integer 1 and the text "1" never collapse.
func canonicalProperties(props map[string]graph.Scalar) string {
	if len(props) == 0 {
		return ""
	}
	keys := make([]string, 0, len(props))
	for key := range props {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var b strings.Builder
	for _, key := range keys {
		value := props[key]
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(string(value.Kind))
		b.WriteByte(':')
		switch value.Kind {
		case graph.Text:
			b.WriteString(strconv.Quote(value.Text))
		case graph.Int:
			b.WriteString(strconv.FormatInt(value.Int, 10))
		case graph.Bool:
			b.WriteString(strconv.FormatBool(value.Bool))
		}
		b.WriteByte(';')
	}
	return b.String()
}
