package catalog

import "rillweave/graph"

type uniqueKey struct {
	kind, property string
	value          graph.Scalar
}

func Validate(g *graph.Graph) error {
	if !graph.ValidID(g.Name) || g.Revision == 0 || g.Vertices == nil || g.Arcs == nil {
		return graph.Fail(graph.Invalid, "graph identity, revision or entity maps are invalid")
	}
	if err := Rules(g.Schema); err != nil {
		return err
	}
	if len(g.Vertices) > graph.MaxVertices || len(g.Arcs) > graph.MaxArcs {
		return graph.Fail(graph.Budget, "graph capacity exceeded")
	}
	unique := make(map[uniqueKey]string)
	for _, id := range graph.Keys(g.Vertices) {
		v := g.Vertices[id]
		rule, ok := g.Schema.Vertices[v.Kind]
		if id != v.ID || !graph.ValidID(id) || !ok {
			return graph.Fail(graph.Invalid, "invalid vertex %q or undeclared kind %q", id, v.Kind)
		}
		if err := values("vertex", id, rule.Properties, v.Properties); err != nil {
			return err
		}
		for _, field := range graph.Keys(v.Properties) {
			if !rule.Properties[field].Unique {
				continue
			}
			key := uniqueKey{v.Kind, field, v.Properties[field]}
			if owner, exists := unique[key]; exists {
				return graph.Fail(graph.Conflict, "vertices %q and %q share a unique %q value", owner, id, field)
			}
			unique[key] = id
		}
	}
	for _, id := range graph.Keys(g.Arcs) {
		if err := validateArc(g, id); err != nil {
			return err
		}
	}
	return nil
}

func validateArc(g *graph.Graph, id string) error {
	a := g.Arcs[id]
	rule, ok := g.Schema.Arcs[a.Kind]
	if id != a.ID || !graph.ValidID(id) || !ok {
		return graph.Fail(graph.Invalid, "invalid arc %q or undeclared kind %q", id, a.Kind)
	}
	if a.Cost <= 0 || a.Cost > graph.MaxCost {
		return graph.Fail(graph.Invalid, "arc %q must have cost in [1,%d]", id, graph.MaxCost)
	}
	from, hasFrom := g.Vertices[a.From]
	to, hasTo := g.Vertices[a.To]
	if !hasFrom || !hasTo || from.Kind != rule.From || to.Kind != rule.To {
		return graph.Fail(graph.Conflict, "arc %q requires existing %q -> %q endpoints", id, rule.From, rule.To)
	}
	return values("arc", id, rule.Properties, a.Properties)
}

func values(entity, id string, rules map[string]graph.PropertyRule, props map[string]graph.Scalar) error {
	for _, field := range graph.Keys(props) {
		value := props[field]
		rule, ok := rules[field]
		if !ok || rule.Kind != value.Kind || !value.Valid() {
			return graph.Fail(graph.Invalid, "%s %q has invalid property %q", entity, id, field)
		}
	}
	for _, field := range graph.Keys(rules) {
		if _, ok := props[field]; !ok && rules[field].Required {
			return graph.Fail(graph.Invalid, "%s %q lacks required property %q", entity, id, field)
		}
	}
	return nil
}
