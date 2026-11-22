package catalog

import "rillweave/graph"

func Rules(s graph.Schema) error {
	if len(s.Vertices) == 0 || len(s.Vertices) > graph.MaxRules || len(s.Arcs) > graph.MaxRules {
		return graph.Fail(graph.Invalid, "schema requires 1..64 vertex kinds and at most 64 arc kinds")
	}
	for _, kind := range graph.Keys(s.Vertices) {
		if !graph.ValidID(kind) {
			return graph.Fail(graph.Invalid, "invalid vertex kind %q", kind)
		}
		if err := propertyRules(kind, s.Vertices[kind].Properties, true); err != nil {
			return err
		}
	}
	for _, kind := range graph.Keys(s.Arcs) {
		rule := s.Arcs[kind]
		if !graph.ValidID(kind) {
			return graph.Fail(graph.Invalid, "invalid arc kind %q", kind)
		}
		if _, present := s.Vertices[rule.From]; !present {
			return graph.Fail(graph.Invalid, "arc kind %q has unknown source kind %q", kind, rule.From)
		}
		if _, present := s.Vertices[rule.To]; !present {
			return graph.Fail(graph.Invalid, "arc kind %q has unknown target kind %q", kind, rule.To)
		}
		if err := propertyRules(kind, rule.Properties, false); err != nil {
			return err
		}
	}
	return nil
}

func propertyRules(kind string, properties map[string]graph.PropertyRule, uniqueOK bool) error {
	if len(properties) > graph.MaxProps {
		return graph.Fail(graph.Invalid, "kind %q has too many property rules", kind)
	}
	for _, key := range graph.Keys(properties) {
		rule := properties[key]
		if !graph.ValidID(key) || !rule.Kind.Valid() {
			return graph.Fail(graph.Invalid, "invalid property rule %q on kind %q", key, kind)
		}
		if rule.Unique && !uniqueOK {
			return graph.Fail(graph.Invalid, "unique rules are supported only on vertex properties")
		}
	}
	return nil
}

// PropertyRule resolves a typed property declared on a vertex kind.
func PropertyRule(s graph.Schema, kind, property string) (graph.PropertyRule, error) {
	rule, ok := s.Vertices[kind]
	if !ok {
		return graph.PropertyRule{}, graph.Fail(graph.Invalid, "unknown vertex kind %q", kind)
	}
	field, ok := rule.Properties[property]
	if !ok {
		return graph.PropertyRule{}, graph.Fail(graph.Invalid, "unknown property %q on vertex kind %q", property, kind)
	}
	return field, nil
}

func LookupRule(s graph.Schema, kind, property string, value graph.Scalar) error {
	field, err := PropertyRule(s, kind, property)
	if err != nil {
		return err
	}
	if field.Kind != value.Kind || !value.Valid() {
		return graph.Fail(graph.Invalid, "property %q requires %s", property, field.Kind)
	}
	return nil
}

// PropertyValues verifies that every requested filter value matches the
// declared scalar kind of the named property.
func PropertyValues(s graph.Schema, kind, property string, values []graph.Scalar) error {
	field, err := PropertyRule(s, kind, property)
	if err != nil {
		return err
	}
	for _, value := range values {
		if field.Kind != value.Kind || !value.Valid() {
			return graph.Fail(graph.Invalid, "property %q requires %s", property, field.Kind)
		}
	}
	return nil
}
