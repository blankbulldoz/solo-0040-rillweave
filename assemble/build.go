package assemble

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"rillweave/catalog"
	"rillweave/graph"
	"rillweave/ingest"
)

// builtVertex remembers how an input vertex was mapped.
type builtVertex struct {
	id   string
	kind string
}

type propTypeMap map[string]map[string]map[graph.Kind]bool

// assembleGraph walks the inputs in their original order and emits one
// graph document. Every value passes through ingest.Classify, so the
// only transformation of a scalar is an owner-authorized explicit cast.
func assembleGraph(name string, idx *index, files []ingest.InputFile, report *Report) (*graph.Graph, error) {
	g := graph.New(name, graph.Schema{
		Vertices: map[string]graph.VertexRule{},
		Arcs:     map[string]graph.ArcRule{},
	})
	vertexByID := map[string]builtVertex{}
	originalKind := map[string]string{}
	vertexPropKinds := propTypeMap{}

	for _, f := range files {
		for ordinal, v := range f.Export.Vertices {
			ref := refKey(f.Path, ordinal)
			if idx.excludeRef[ref] == "exclude_vertex" {
				report.Excluded = append(report.Excluded, Disposition{Ref: ref, Reason: "exclude_vertex", Detail: v.ID})
				continue
			}
			doc, err := idx.buildVertex(ref, v, report)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", ref, err)
			}
			if _, exists := g.Vertices[doc.ID]; exists {
				return nil, fmt.Errorf("vertex id %q collides after decisions; add a rename_vertex action", doc.ID)
			}
			if doc.ID != v.ID {
				report.Renamed = append(report.Renamed, Disposition{Ref: ref, Reason: "rename_vertex", Detail: v.ID + " -> " + doc.ID})
			}
			if doc.Kind != v.Kind {
				report.Renamed = append(report.Renamed, Disposition{Ref: ref, Reason: "rename_vertex_kind", Detail: v.Kind + " -> " + doc.Kind})
			}
			g.Vertices[doc.ID] = doc
			// Arcs reference the original export id; the renamed id is
			// a fallback for records that themselves were renamed.
			vertexByID[v.ID] = builtVertex{doc.ID, doc.Kind}
			originalKind[v.ID] = v.Kind
			rememberProps(vertexPropKinds, doc.Kind, doc.Properties)
		}
	}
	// Renamed ids also resolve, after original ids take precedence.
	for _, bv := range g.Vertices {
		if _, ok := vertexByID[bv.ID]; !ok {
			vertexByID[bv.ID] = builtVertex{bv.ID, bv.Kind}
		}
		if _, ok := originalKind[bv.ID]; !ok {
			originalKind[bv.ID] = bv.Kind
		}
	}

	arcPropKinds := propTypeMap{}
	pairByArcKind := map[string]string{}

	for _, f := range files {
		for ordinal, arc := range f.Export.Arcs {
			ref := refKey(f.Path, ordinal)
			if idx.excludeRef[ref] == "exclude_arc" {
				report.Excluded = append(report.Excluded, Disposition{Ref: ref, Reason: "exclude_arc", Detail: arc.ID})
				continue
			}
			from, fromOK := vertexByID[arc.From]
			to, toOK := vertexByID[arc.To]
			if !fromOK || !toOK {
				if idx.excludeDangling[arc.Kind] {
					report.Excluded = append(report.Excluded, Disposition{Ref: ref, Reason: "exclude_dangling_arc", Detail: arc.ID})
					continue
				}
				return nil, fmt.Errorf("%s: arc %q references missing endpoint; add an exclude_dangling_arc action", ref, arc.ID)
			}
			origFromKind := originalKind[arc.From]
			origToKind := originalKind[arc.To]
			if idx.excludePair[pairKey(arc.Kind, origFromKind, origToKind)] {
				report.Excluded = append(report.Excluded, Disposition{Ref: ref, Reason: "exclude_arc_pair", Detail: arc.ID})
				continue
			}
			doc, err := idx.buildArc(ref, arc, from, to, origFromKind, origToKind, arcPropKinds, report)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", ref, err)
			}
			if _, exists := g.Arcs[doc.ID]; exists {
				return nil, fmt.Errorf("arc id %q collides after decisions; add a rename_arc action", doc.ID)
			}
			if doc.ID != arc.ID {
				report.Renamed = append(report.Renamed, Disposition{Ref: ref, Reason: "rename_arc", Detail: arc.ID + " -> " + doc.ID})
			}
			if doc.Kind != arc.Kind {
				report.Renamed = append(report.Renamed, Disposition{Ref: ref, Reason: "rename_arc_kind", Detail: arc.Kind + " -> " + doc.Kind})
			}
			g.Arcs[doc.ID] = doc
			pair := pairKey(doc.Kind, from.kind, to.kind)
			if prev, seen := pairByArcKind[doc.Kind]; seen && prev != pair {
				return nil, fmt.Errorf("arc kind %q still connects multiple endpoint pairs after decisions; add split_arc_kind actions", doc.Kind)
			}
			pairByArcKind[doc.Kind] = pair
		}
	}

	buildSchema(g, vertexPropKinds, arcPropKinds, pairByArcKind)
	report.Included = Included{Vertices: len(g.Vertices), Arcs: len(g.Arcs)}
	return g, nil
}

func (i *index) buildVertex(ref string, v ingest.RawVertex, report *Report) (graph.Vertex, error) {
	kind := i.resolveVertexKind(v)
	if !graph.ValidID(kind) {
		return graph.Vertex{}, fmt.Errorf("resulting vertex kind %q is not a valid identifier", kind)
	}
	id := v.ID
	if renamed, ok := i.vertexIDRename[ref]; ok {
		id = renamed
	}
	if !graph.ValidID(id) {
		return graph.Vertex{}, fmt.Errorf("resulting vertex id %q is not a valid identifier", id)
	}
	props, err := i.mapProperties(ref, v.Kind, kind, v.Properties, true, report)
	if err != nil {
		return graph.Vertex{}, err
	}
	return graph.Vertex{ID: id, Kind: kind, Properties: props}, nil
}

// resolveVertexKind applies kind-level renames before any property-driven
// split, so that "Service" and "service" converge on one final kind.
func (i *index) resolveVertexKind(v ingest.RawVertex) string {
	base := v.Kind
	if renamed, ok := i.vertexKindRename[base]; ok {
		base = renamed
	}
	for key, suffix := range i.vertexKindSplit {
		if key.Kind != v.Kind {
			continue
		}
		raw, present := v.Properties[key.Property]
		if !present {
			continue
		}
		k, _, problem := ingest.Classify(raw)
		if problem != "" {
			continue
		}
		return sanitizeName(base + suffix + "_" + string(k))
	}
	return base
}

func (i *index) buildArc(ref string, arc ingest.RawArc, from, to builtVertex,
	origFromKind, origToKind string, arcPropKinds propTypeMap, report *Report) (graph.Arc, error) {
	kind := arc.Kind
	if split, ok := i.arcPairSplit[pairKey(arc.Kind, origFromKind, origToKind)]; ok {
		kind = split
	} else if renamed, ok := i.arcKindRename[arc.Kind]; ok {
		kind = renamed
	}
	if !graph.ValidID(kind) {
		return graph.Arc{}, fmt.Errorf("resulting arc kind %q is not a valid identifier", kind)
	}
	scalarKind, value, problem := ingest.Classify(arc.Cost)
	if problem != "" || scalarKind != graph.Int || value.Int <= 0 || value.Int > graph.MaxCost {
		return graph.Arc{}, fmt.Errorf("arc %q has no valid positive integer cost; exclude it or repair the export", arc.ID)
	}
	id := arc.ID
	if renamed, ok := i.arcIDRename[ref]; ok {
		id = renamed
	}
	if !graph.ValidID(id) {
		return graph.Arc{}, fmt.Errorf("resulting arc id %q is not a valid identifier", id)
	}
	props, err := i.mapProperties(ref, arc.Kind, kind, arc.Properties, false, report)
	if err != nil {
		return graph.Arc{}, err
	}
	rememberProps(arcPropKinds, kind, props)
	return graph.Arc{ID: id, Kind: kind, From: from.id, To: to.id, Cost: value.Int, Properties: props}, nil
}

// mapProperties rewrites property names per decisions and classifies
// every value. Unsupported values that were not explicitly dropped fail
// the build instead of disappearing. When a kind rename merges records
// into another kind, property rules defined on the destination kind
// also apply to the merged records.
func (i *index) mapProperties(ref, originalKind, finalKind string,
	raw map[string]json.RawMessage, vertex bool, report *Report) (map[string]graph.Scalar, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	dropped := i.dropProperty[ref]
	out := map[string]graph.Scalar{}
	for name, token := range raw {
		if dropped != nil {
			if _, drop := dropped[name]; drop {
				report.DroppedValues = append(report.DroppedValues,
					Disposition{Ref: ref, Reason: "drop_property", Detail: name})
				continue
			}
		}
		k, value, problem := ingest.Classify(token)
		if problem != "" {
			return nil, fmt.Errorf("property %q still has an unsupported value (%s); add a drop_property or exclude action", name, problem)
		}
		finalName := i.mapPropertyName(originalKind, finalKind, name, k, &value, vertex)
		if finalName != name {
			report.Renamed = append(report.Renamed,
				Disposition{Ref: ref, Reason: "rename_property", Detail: name + " -> " + finalName})
		}
		if !graph.ValidID(finalName) {
			return nil, fmt.Errorf("resulting property name %q is not a valid identifier", finalName)
		}
		if _, clash := out[finalName]; clash {
			return nil, fmt.Errorf("property mappings for %q collide on %q", name, finalName)
		}
		out[finalName] = value
	}
	return out, nil
}

func (i *index) mapPropertyName(originalKind, finalKind, name string, k graph.Kind,
	value *graph.Scalar, vertex bool) string {
	lookup := func(kind string) (string, bool) {
		key := kindProp{kind, name}
		if vertex {
			if routes, ok := i.vertexPropSplit[key]; ok {
				if routed, ok := routes[k]; ok {
					return routed, true
				}
			}
			if renamed, ok := i.vertexPropRename[key]; ok {
				return renamed, true
			}
		} else {
			if routes, ok := i.arcPropSplit[key]; ok {
				if routed, ok := routes[k]; ok {
					return routed, true
				}
			}
			if renamed, ok := i.arcPropRename[key]; ok {
				return renamed, true
			}
		}
		if rule, ok := i.cast[key]; ok {
			if converted, err := explicitConvert(*value, rule.target); err == nil {
				*value = converted
				return name, true
			}
		}
		return "", false
	}
	if routed, ok := lookup(originalKind); ok {
		return routed
	}
	if finalKind != originalKind {
		if routed, ok := lookup(finalKind); ok {
			return routed
		}
	}
	return name
}

// explicitConvert performs an owner-authorized, audited conversion. The
// authorization note is stored on the decisions document; nothing here
// runs implicitly during inference.
func explicitConvert(v graph.Scalar, target graph.Kind) (graph.Scalar, error) {
	if v.Kind == target {
		return v, nil
	}
	switch target {
	case graph.Text:
		switch v.Kind {
		case graph.Int:
			return graph.Scalar{Kind: graph.Text, Text: strconv.FormatInt(v.Int, 10)}, nil
		case graph.Bool:
			return graph.Scalar{Kind: graph.Text, Text: strconv.FormatBool(v.Bool)}, nil
		}
	case graph.Int:
		if v.Kind == graph.Text {
			parsed, err := strconv.ParseInt(strings.TrimSpace(v.Text), 10, 64)
			if err != nil {
				return graph.Scalar{}, fmt.Errorf("text %q cannot be parsed as int64", v.Text)
			}
			return graph.Scalar{Kind: graph.Int, Int: parsed}, nil
		}
	case graph.Bool:
		if v.Kind == graph.Text {
			switch strings.TrimSpace(v.Text) {
			case "true":
				return graph.Scalar{Kind: graph.Bool, Bool: true}, nil
			case "false":
				return graph.Scalar{Kind: graph.Bool, Bool: false}, nil
			}
			return graph.Scalar{}, fmt.Errorf("text %q cannot be parsed as boolean", v.Text)
		}
	}
	return graph.Scalar{}, fmt.Errorf("no authorized conversion from %s to %s", v.Kind, target)
}

func rememberProps(store propTypeMap, kind string, props map[string]graph.Scalar) {
	for name, scalar := range props {
		if store[kind] == nil {
			store[kind] = map[string]map[graph.Kind]bool{}
		}
		if store[kind][name] == nil {
			store[kind][name] = map[graph.Kind]bool{}
		}
		store[kind][name][scalar.Kind] = true
	}
}

// buildSchema declares one rule per observed kind and property. The
// property universe is taken from the final assembled records, so a
// property whose every value was split or dropped never gets a rule.
// All properties start optional and non-unique; required/unique flags
// come from the owner decisions and are applied by applyRuleFlags.
func buildSchema(g *graph.Graph, vertexProps, arcProps propTypeMap, pairByArcKind map[string]string) {
	for _, id := range sortedKeys(g.Vertices) {
		vertex := g.Vertices[id]
		rule, exists := g.Schema.Vertices[vertex.Kind]
		if !exists {
			rule = graph.VertexRule{Properties: map[string]graph.PropertyRule{}}
		}
		for name := range vertex.Properties {
			if _, declared := rule.Properties[name]; declared {
				continue
			}
			kinds := vertexProps[vertex.Kind][name]
			if len(kinds) != 1 {
				continue // unresolved conflict; catalog validation reports it
			}
			rule.Properties[name] = graph.PropertyRule{Kind: soleKind(kinds)}
		}
		g.Schema.Vertices[vertex.Kind] = rule
	}
	for _, id := range sortedKeys(g.Arcs) {
		arc := g.Arcs[id]
		pair := pairByArcKind[arc.Kind]
		parts := strings.SplitN(pair, "|", 3)
		rule, exists := g.Schema.Arcs[arc.Kind]
		if !exists {
			rule = graph.ArcRule{From: parts[1], To: parts[2], Properties: map[string]graph.PropertyRule{}}
		}
		for name := range arc.Properties {
			if _, declared := rule.Properties[name]; declared {
				continue
			}
			kinds := arcProps[arc.Kind][name]
			if len(kinds) != 1 {
				continue
			}
			rule.Properties[name] = graph.PropertyRule{Kind: soleKind(kinds)}
		}
		g.Schema.Arcs[arc.Kind] = rule
	}
}

// applyRuleFlags adds owner-confirmed required/unique declarations after
// schema assembly. Unknown names fail loudly rather than being ignored.
func applyRuleFlags(g *graph.Graph, d *Decisions) {
	for kind, names := range d.RequiredVertex {
		rule := g.Schema.Vertices[kind]
		for _, name := range names {
			if pr, ok := rule.Properties[name]; ok {
				pr.Required = true
				rule.Properties[name] = pr
			}
		}
		g.Schema.Vertices[kind] = rule
	}
	for kind, names := range d.UniqueVertex {
		rule := g.Schema.Vertices[kind]
		for _, name := range names {
			if pr, ok := rule.Properties[name]; ok {
				pr.Unique = true
				rule.Properties[name] = pr
			}
		}
		g.Schema.Vertices[kind] = rule
	}
}

func soleKind(m map[graph.Kind]bool) graph.Kind {
	for k := range m {
		return k
	}
	return ""
}

func refKey(file string, ordinal int) string {
	return fmt.Sprintf("%s#%d", file, ordinal)
}

// catalogValidate runs the exact validator used by the service before
// publication, so an emitted image is installable without surprises.
func catalogValidate(g *graph.Graph) error {
	return catalog.Validate(g)
}

func sanitizeName(name string) string {
	var b []byte
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'A' && c <= 'Z':
			b = append(b, c+('a'-'A'))
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == '-':
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	if len(b) == 0 || !((b[0] >= 'a' && b[0] <= 'z') || (b[0] >= '0' && b[0] <= '9')) {
		b = append([]byte{'k'}, b...)
	}
	if len(b) > 64 {
		b = b[:64]
	}
	return string(b)
}
