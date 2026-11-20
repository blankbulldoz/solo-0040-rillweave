package change

import (
	"maps"

	"rillweave/graph"
)

type Entities struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Updated []string `json:"updated"`
}

type Difference struct {
	From          uint64   `json:"from"`
	To            uint64   `json:"to"`
	Vertices      Entities `json:"vertices"`
	Arcs          Entities `json:"arcs"`
	SchemaChanged bool     `json:"schema_changed"`
}

func Compare(from, to *graph.Graph) Difference {
	return Difference{
		From: from.Revision, To: to.Revision,
		Vertices: diff(from.Vertices, to.Vertices, sameVertex),
		Arcs: diff(from.Arcs, to.Arcs, sameArc),
		SchemaChanged: !sameSchema(from.Schema, to.Schema),
	}
}

func diff[V any](from, to map[string]V, equal func(V, V) bool) Entities {
	result := Entities{Added: []string{}, Removed: []string{}, Updated: []string{}}
	for _, id := range graph.Keys(to) {
		old, ok := from[id]
		if !ok {
			result.Added = append(result.Added, id)
		} else if !equal(old, to[id]) {
			result.Updated = append(result.Updated, id)
		}
	}
	for _, id := range graph.Keys(from) {
		if _, ok := to[id]; !ok {
			result.Removed = append(result.Removed, id)
		}
	}
	return result
}

func sameVertex(a, b graph.Vertex) bool {
	return a.Kind == b.Kind && maps.Equal(a.Properties, b.Properties)
}

func sameArc(a, b graph.Arc) bool {
	return a.Kind == b.Kind && a.From == b.From && a.To == b.To && a.Cost == b.Cost &&
		maps.Equal(a.Properties, b.Properties)
}

func sameSchema(a, b graph.Schema) bool {
	return maps.EqualFunc(a.Vertices, b.Vertices, func(a, b graph.VertexRule) bool {
		return maps.Equal(a.Properties, b.Properties)
	}) && maps.EqualFunc(a.Arcs, b.Arcs, func(a, b graph.ArcRule) bool {
		return a.From == b.From && a.To == b.To && maps.Equal(a.Properties, b.Properties)
	})
}
