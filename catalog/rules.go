package catalog

import (
	"rillweave/graph"
	"rillweave/index"
)

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

func LookupRule(s graph.Schema, kind, property string, value graph.Scalar) error {
	rule, ok := s.Vertices[kind]
	if !ok {
		return graph.Fail(graph.Invalid, "unknown vertex kind %q", kind)
	}
	field, ok := rule.Properties[property]
	if !ok {
		return graph.Fail(graph.Invalid, "unknown property %q on vertex kind %q", property, kind)
	}
	if field.Kind != value.Kind || !value.Valid() {
		return graph.Fail(graph.Invalid, "property %q requires %s", property, field.Kind)
	}
	return nil
}

// LookupPlan validates every condition and set operation before any posting
// is read. The same property name may carry different scalar kinds on
// different vertex kinds; each condition is checked against its own kind's
// catalog. Validation is all-or-nothing: an illegal right-hand term is
// rejected even when an earlier subtraction has already made the running set
// empty, so callers cannot bypass type rules through query shape.
func LookupPlan(s graph.Schema, terms []index.Term) error {
	if len(terms) == 0 {
		return graph.Fail(graph.Invalid, "a composite lookup requires 1..%d conditions", index.MaxLookupConditions)
	}
	if len(terms) > index.MaxLookupConditions {
		return graph.Fail(graph.Invalid, "a composite lookup accepts at most %d conditions", index.MaxLookupConditions)
	}
	for i, term := range terms {
		if term.Op != "" && !term.Op.Valid() {
			return graph.Fail(graph.Invalid, "condition %d requires op union, intersect or subtract", i+1)
		}
		if i > 0 && term.Op == "" {
			return graph.Fail(graph.Invalid, "condition %d requires op union, intersect or subtract", i+1)
		}
		if err := LookupRule(s, term.Kind, term.Property, term.Value); err != nil {
			return graph.Fail(graph.Invalid, "condition %d: %v", i+1, err)
		}
	}
	return nil
}
