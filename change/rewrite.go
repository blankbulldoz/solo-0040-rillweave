package change

import (
	"slices"

	"rillweave/graph"
)

// MaxWitness bounds how many unresolved entities a rejection reports. The
// total count is always included so an operator knows the remainder.
const MaxWitness = 64

// Match is one typed property condition. A missing property never satisfies a
// condition, so absent values remain distinct from zero values.
type Match struct {
	Property string       `json:"property"`
	Equals   graph.Scalar `json:"equals"`
}

func (m Match) holds(properties map[string]graph.Scalar) bool {
	value, present := properties[m.Property]
	return present && value == m.Equals
}

// VertexClass names a target vertex kind and the conditions an instance of the
// rewritten kind must satisfy. An empty condition list is rejected; use
// Rewrite.Default for the remaining instances.
type VertexClass struct {
	Kind string  `json:"kind"`
	When []Match `json:"when"`
}

func (c VertexClass) matches(properties map[string]graph.Scalar) bool {
	for _, condition := range c.When {
		if !condition.holds(properties) {
			return false
		}
	}
	return true
}

// ArcTarget is one allowed replacement for an arc kind. When is optional and
// tests the arc's own properties; endpoint compatibility always uses the
// target schema.
type ArcTarget struct {
	Kind string  `json:"kind"`
	When []Match `json:"when,omitempty"`
}

func (t ArcTarget) matches(properties map[string]graph.Scalar) bool {
	for _, condition := range t.When {
		if !condition.holds(properties) {
			return false
		}
	}
	return true
}

// ArcRule converts every arc of Kind whose new endpoint kinds make exactly one
// of To legal. Arcs of kinds not listed here are not rewritten.
type ArcRule struct {
	Kind string      `json:"kind"`
	To   []ArcTarget `json:"to"`
}

// Rewrite is the payload of a rewrite_kind edit. It retypes every vertex of
// Vertex according to Classes/Default and converts listed arc kinds according
// to ArcRule. IDs, property values and unlisted arc kinds are left alone.
type Rewrite struct {
	Vertex  string        `json:"vertex"`
	Classes []VertexClass `json:"classes"`
	Default string        `json:"default,omitempty"`
	Arcs    []ArcRule     `json:"arcs,omitempty"`
}

func (r Rewrite) validate(schema graph.Schema) error {
	if !graph.ValidID(r.Vertex) {
		return graph.Fail(graph.Invalid, "rewrite requires a valid vertex kind")
	}
	if len(r.Classes) == 0 {
		return graph.Fail(graph.Invalid, "rewrite of %q requires at least one class", r.Vertex)
	}
	if len(r.Classes) > graph.MaxRules {
		return graph.Fail(graph.Invalid, "rewrite of %q has too many classes", r.Vertex)
	}
	classKinds := make(map[string]bool, len(r.Classes))
	for _, class := range r.Classes {
		if !graph.ValidID(class.Kind) {
			return graph.Fail(graph.Invalid, "rewrite class has invalid vertex kind %q", class.Kind)
		}
		if _, declared := schema.Vertices[class.Kind]; !declared {
			return graph.Fail(graph.Invalid, "rewrite class %q is not declared by the candidate schema", class.Kind)
		}
		if len(class.When) == 0 {
			return graph.Fail(graph.Invalid, "rewrite class %q requires a when condition; use default for the remainder", class.Kind)
		}
		if err := conditionsMatch(schema.Vertices[r.Vertex].Properties, class.Kind, class.When); err != nil {
			return err
		}
		if classKinds[class.Kind] {
			return graph.Fail(graph.Invalid, "rewrite of %q lists class %q twice", r.Vertex, class.Kind)
		}
		classKinds[class.Kind] = true
	}
	if r.Default != "" {
		if !graph.ValidID(r.Default) {
			return graph.Fail(graph.Invalid, "rewrite default has invalid vertex kind %q", r.Default)
		}
		if _, declared := schema.Vertices[r.Default]; !declared {
			return graph.Fail(graph.Invalid, "rewrite default %q is not declared by the candidate schema", r.Default)
		}
	}
	if len(r.Arcs) > graph.MaxRules {
		return graph.Fail(graph.Invalid, "rewrite of %q has too many arc rules", r.Vertex)
	}
	seen := make(map[string]bool, len(r.Arcs))
	for _, rule := range r.Arcs {
		if !graph.ValidID(rule.Kind) {
			return graph.Fail(graph.Invalid, "rewrite has invalid arc kind %q", rule.Kind)
		}
		if seen[rule.Kind] {
			return graph.Fail(graph.Invalid, "rewrite lists arc kind %q more than once", rule.Kind)
		}
		seen[rule.Kind] = true
		if len(rule.To) == 0 {
			return graph.Fail(graph.Invalid, "rewrite of arc kind %q requires at least one target", rule.Kind)
		}
		targets := make(map[string]bool, len(rule.To))
		for _, target := range rule.To {
			if !graph.ValidID(target.Kind) {
				return graph.Fail(graph.Invalid, "rewrite of arc kind %q has invalid target %q", rule.Kind, target.Kind)
			}
			if targets[target.Kind] {
				return graph.Fail(graph.Invalid, "rewrite of arc kind %q lists target %q twice", rule.Kind, target.Kind)
			}
			targets[target.Kind] = true
			if _, declared := schema.Arcs[target.Kind]; !declared {
				return graph.Fail(graph.Invalid, "rewrite target %q is not declared by the candidate schema", target.Kind)
			}
			if err := conditionsMatch(schema.Arcs[target.Kind].Properties, target.Kind, target.When); err != nil {
				return err
			}
		}
	}
	return nil
}

func conditionsMatch(declared map[string]graph.PropertyRule, kind string, conditions []Match) error {
	for _, condition := range conditions {
		if !graph.ValidID(condition.Property) || !condition.Equals.Valid() {
			return graph.Fail(graph.Invalid, "rewrite condition on %q is invalid", kind)
		}
		rule, present := declared[condition.Property]
		if !present {
			return graph.Fail(graph.Invalid,
				"rewrite condition on %q names property %q that its records cannot carry", kind, condition.Property)
		}
		if rule.Kind != condition.Equals.Kind {
			return graph.Fail(graph.Invalid,
				"rewrite condition on %q equates property %q with the wrong type", kind, condition.Property)
		}
	}
	return nil
}

// RewriteReport is the deterministic plan produced by one rewrite edit.
type RewriteReport struct {
	Vertex        string          `json:"vertex"`
	Default       string          `json:"default,omitempty"`
	Vertices      []VertexRewrite `json:"vertices"`
	Arcs          []ArcRewrite    `json:"arcs"`
	PreservedArcs []string        `json:"preserved_arcs"`
}

type VertexRewrite struct {
	ID   string `json:"id"`
	From string `json:"from"`
	To   string `json:"to"`
}

type ArcRewrite struct {
	ID         string `json:"id"`
	From       string `json:"from"`
	To         string `json:"to"`
	FromVertex string `json:"from_vertex"`
	ToVertex   string `json:"to_vertex"`
}

// RewriteWitness carries the endpoint material needed to disambiguate a
// rewrite that cannot decide a unique legal outcome.
type RewriteWitness struct {
	Vertex    string           `json:"vertex"`
	Vertices  []VertexEvidence `json:"vertices,omitempty"`
	Links     []LinkEvidence   `json:"links,omitempty"`
	Total     int              `json:"total"`
	Truncated bool             `json:"truncated"`
}

type VertexEvidence struct {
	ID           string                  `json:"id"`
	Kind         string                  `json:"kind"`
	Reason       string                  `json:"reason"`
	MatchedKinds []string                `json:"matched_kinds,omitempty"`
	Properties   map[string]graph.Scalar `json:"properties,omitempty"`
}

type EndpointEvidence struct {
	ID            string                  `json:"id"`
	Present       bool                    `json:"present"`
	Kind          string                  `json:"kind,omitempty"`
	NextKind      string                  `json:"next_kind,omitempty"`
	PossibleKinds []string                `json:"possible_kinds,omitempty"`
	Properties    map[string]graph.Scalar `json:"properties,omitempty"`
}

type CandidateEvidence struct {
	Kind            string `json:"kind"`
	Declared        bool   `json:"declared"`
	From            string `json:"from,omitempty"`
	To              string `json:"to,omitempty"`
	EndpointsMatch  bool   `json:"endpoints_match"`
	ConditionsMatch bool   `json:"conditions_match"`
}

type LinkEvidence struct {
	Arc        string                  `json:"arc"`
	Kind       string                  `json:"kind"`
	Cost       int64                   `json:"cost"`
	Reason     string                  `json:"reason"`
	From       EndpointEvidence        `json:"from_endpoint"`
	To         EndpointEvidence        `json:"to_endpoint"`
	Properties map[string]graph.Scalar `json:"properties,omitempty"`
	Candidates []CandidateEvidence     `json:"candidates,omitempty"`
}

type classification struct {
	next     map[string]string
	possible map[string][]string
	problems []VertexEvidence
}

func (r Rewrite) classify(g *graph.Graph) classification {
	result := classification{
		next:     make(map[string]string),
		possible: make(map[string][]string),
	}
	for _, id := range graph.Keys(g.Vertices) {
		vertex := g.Vertices[id]
		if vertex.Kind != r.Vertex {
			continue
		}
		matched := make(map[string]bool)
		for _, class := range r.Classes {
			if class.matches(vertex.Properties) {
				matched[class.Kind] = true
			}
		}
		switch len(matched) {
		case 1:
			for kind := range matched {
				result.next[id] = kind
			}
		case 0:
			if r.Default != "" {
				result.next[id] = r.Default
				break
			}
			result.problems = append(result.problems, VertexEvidence{
				ID: id, Kind: vertex.Kind, Reason: "unclassified", Properties: vertex.Properties,
			})
		default:
			kinds := make([]string, 0, len(matched))
			for kind := range matched {
				kinds = append(kinds, kind)
			}
			slices.Sort(kinds)
			result.possible[id] = kinds
			result.problems = append(result.problems, VertexEvidence{
				ID: id, Kind: vertex.Kind, Reason: "ambiguous",
				MatchedKinds: kinds, Properties: vertex.Properties,
			})
		}
	}
	return result
}

func (r Rewrite) apply(g *graph.Graph) (RewriteReport, error) {
	if err := r.validate(g.Schema); err != nil {
		return RewriteReport{}, err
	}
	classified := r.classify(g)
	if len(classified.problems) > 0 {
		return RewriteReport{}, r.rejectVertices(g, classified)
	}
	report := RewriteReport{
		Vertex: r.Vertex, Default: r.Default,
		Vertices: []VertexRewrite{}, Arcs: []ArcRewrite{}, PreservedArcs: []string{},
	}
	originals := make(map[string]string, len(classified.next))
	for _, id := range graph.Keys(classified.next) {
		vertex := g.Vertices[id]
		originals[id] = vertex.Kind
		vertex.Kind = classified.next[id]
		g.Vertices[id] = vertex
		report.Vertices = append(report.Vertices, VertexRewrite{id, originals[id], vertex.Kind})
	}
	rules := make(map[string][]ArcTarget, len(r.Arcs))
	for _, rule := range r.Arcs {
		rules[rule.Kind] = rule.To
	}
	var unresolved []LinkEvidence
	converted := []ArcRewrite{}
	preserved := []string{}
	for _, id := range graph.Keys(g.Arcs) {
		arc := g.Arcs[id]
		targets, listed := rules[arc.Kind]
		if !listed {
			_, movedFrom := classified.next[arc.From]
			_, movedTo := classified.next[arc.To]
			if !movedFrom && !movedTo {
				continue
			}
			from := kindOf(g, classified.next, arc.From)
			to := kindOf(g, classified.next, arc.To)
			rule, declared := g.Schema.Arcs[arc.Kind]
			if !declared || rule.From != from || rule.To != to {
				unresolved = append(unresolved, LinkEvidence{
					Arc: id, Kind: arc.Kind, Cost: arc.Cost, Reason: "unlisted_endpoint_mismatch",
					From: evidence(g, originals, classified, arc.From),
					To:   evidence(g, originals, classified, arc.To), Properties: arc.Properties,
				})
				continue
			}
			preserved = append(preserved, id)
			continue
		}
		from := kindOf(g, classified.next, arc.From)
		to := kindOf(g, classified.next, arc.To)
		candidates := make([]CandidateEvidence, 0, len(targets))
		eligible := make([]string, 0, len(targets))
		for _, target := range targets {
			rule, declared := g.Schema.Arcs[target.Kind]
			endpoints := declared && rule.From == from && rule.To == to
			conditions := target.matches(arc.Properties)
			candidates = append(candidates, CandidateEvidence{
				Kind: target.Kind, Declared: declared,
				From: from, To: to,
				EndpointsMatch: endpoints, ConditionsMatch: conditions,
			})
			if endpoints && conditions {
				eligible = append(eligible, target.Kind)
			}
		}
		switch len(eligible) {
		case 1:
			next := arc
			next.Kind = eligible[0]
			if next.Kind == arc.Kind {
				preserved = append(preserved, id)
				continue
			}
			g.Arcs[id] = next
			converted = append(converted, ArcRewrite{
				ID: id, From: arc.Kind, To: next.Kind, FromVertex: arc.From, ToVertex: arc.To,
			})
		case 0:
			unresolved = append(unresolved, LinkEvidence{
				Arc: id, Kind: arc.Kind, Cost: arc.Cost, Reason: "no_compatible_kind",
				From:       evidence(g, originals, classified, arc.From),
				To:         evidence(g, originals, classified, arc.To),
				Properties: arc.Properties, Candidates: candidates,
			})
		default:
			unresolved = append(unresolved, LinkEvidence{
				Arc: id, Kind: arc.Kind, Cost: arc.Cost, Reason: "ambiguous_kind",
				From:       evidence(g, originals, classified, arc.From),
				To:         evidence(g, originals, classified, arc.To),
				Properties: arc.Properties, Candidates: candidates,
			})
		}
	}
	if len(unresolved) > 0 {
		return RewriteReport{}, rejectLinks(r.Vertex, unresolved)
	}
	report.Arcs = converted
	report.PreservedArcs = preserved
	return report, nil
}

func (r Rewrite) rejectVertices(g *graph.Graph, classified classification) error {
	problems := make(map[string]bool, len(classified.problems))
	for _, problem := range classified.problems {
		problems[problem.ID] = true
	}
	links := []LinkEvidence{}
	for _, id := range graph.Keys(g.Arcs) {
		arc := g.Arcs[id]
		if !problems[arc.From] && !problems[arc.To] {
			continue
		}
		links = append(links, LinkEvidence{
			Arc: id, Kind: arc.Kind, Cost: arc.Cost, Reason: "endpoint_unresolved",
			From: evidence(g, nil, classified, arc.From),
			To:   evidence(g, nil, classified, arc.To), Properties: arc.Properties,
		})
	}
	witness := compactWitness(r.Vertex, classified.problems, links)
	return graph.FailWitness(graph.Conflict, witness,
		"rewrite of vertex kind %q cannot classify %d vertices; inspect endpoint material and refine classes or default",
		r.Vertex, len(classified.problems))
}

func rejectLinks(vertex string, unresolved []LinkEvidence) error {
	witness := compactWitness(vertex, nil, unresolved)
	return graph.FailWitness(graph.Conflict, witness,
		"rewrite cannot decide a unique legal arc kind for %d connections; inspect endpoint material and refine arc rules",
		witness.Total)
}

// compactWitness trims vertices and links under one shared bound so the total
// remains meaningful, and always reports the untrimmed count.
func compactWitness(vertex string, vertices []VertexEvidence, links []LinkEvidence) RewriteWitness {
	witness := RewriteWitness{Vertex: vertex, Total: len(vertices) + len(links)}
	budget := MaxWitness
	if len(vertices) > budget {
		vertices = vertices[:budget]
		witness.Truncated = true
	}
	budget -= len(vertices)
	if len(links) > budget {
		links = links[:budget]
		witness.Truncated = true
	}
	witness.Vertices = vertices
	witness.Links = links
	return witness
}

func kindOf(g *graph.Graph, next map[string]string, id string) string {
	if kind, ok := next[id]; ok {
		return kind
	}
	return g.Vertices[id].Kind
}

// evidence describes an endpoint using the state before the rewrite and the
// kind it resolves to, which is what lets an operator disambiguate.
func evidence(g *graph.Graph, originals map[string]string, classified classification, id string) EndpointEvidence {
	result := EndpointEvidence{ID: id, PossibleKinds: classified.possible[id]}
	vertex, present := g.Vertices[id]
	result.Present = present
	if !present {
		return result
	}
	result.Properties = vertex.Properties
	if kind, ok := originals[id]; ok {
		result.Kind = kind
	} else {
		result.Kind = vertex.Kind
	}
	if kind, ok := classified.next[id]; ok {
		result.NextKind = kind
	} else {
		result.NextKind = result.Kind
	}
	return result
}
