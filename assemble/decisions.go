// Package assemble applies a data-owner decisions document to the scanned
// exports and produces an installable graph. It reuses the real catalog
// and image packages, so an emitted image is accepted by the same
// validation path as a service-side restore.
package assemble

import (
	"fmt"
	"sort"
	"strings"

	"rillweave/graph"
	"rillweave/ingest"
	"rillweave/profile"
)

// Action is one confirmed transformation. Actions come from the starter
// decisions document generated beside the profile and may be edited;
// unknown or incomplete actions are rejected rather than guessed.
type Action struct {
	Op string `json:"op"`

	// Routing selectors.
	EntityKind   string `json:"entity_kind,omitempty"`
	ArcKind      string `json:"arc_kind,omitempty"`
	Property     string `json:"property,omitempty"`
	ObservedKind string `json:"observed_kind,omitempty"`
	Suffix       string `json:"suffix,omitempty"`
	NewName      string `json:"new_name,omitempty"`
	From         string `json:"from,omitempty"`
	To           string `json:"to,omitempty"`

	// Record-level actions keyed by file#ordinal.
	ScopeRef string `json:"scope_ref,omitempty"`

	// explicit_cast requires an owner note naming the target kind;
	// NewName carries the target scalar kind for that op.
	OwnerNote string `json:"owner_note,omitempty"`
}

// Decisions is the owner-editable confirmation document.
type Decisions struct {
	Version        int                 `json:"version"`
	ProfileDigest  string              `json:"profile_digest"`
	GraphName      string              `json:"graph_name"`
	Actions        []Action            `json:"actions"`
	RequiredVertex map[string][]string `json:"required_vertex_properties,omitempty"`
	UniqueVertex   map[string][]string `json:"unique_vertex_properties,omitempty"`
	Notes          []string            `json:"notes,omitempty"`
}

// Report summarizes a build and records every record-level disposition.
type Report struct {
	GraphName     string        `json:"graph_name"`
	ProfileDigest string        `json:"profile_digest"`
	Included      Included      `json:"included"`
	Excluded      []Disposition `json:"excluded"`
	DroppedValues []Disposition `json:"dropped_values"`
	Renamed       []Disposition `json:"renamed"`
	Schema        graph.Schema  `json:"schema"`
	CatalogCheck  string        `json:"catalog_check"`
}

// Included reports final entity counts.
type Included struct {
	Vertices int `json:"vertices"`
	Arcs     int `json:"arcs"`
}

// Disposition explains what happened to one input record or value.
type Disposition struct {
	Ref    string `json:"ref"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

type index struct {
	vertexKindRename map[string]string
	arcKindRename    map[string]string
	vertexPropRename map[kindProp]string
	arcPropRename    map[kindProp]string
	vertexPropSplit  map[kindProp]map[graph.Kind]string
	arcPropSplit     map[kindProp]map[graph.Kind]string
	vertexKindSplit  map[kindProp]string
	vertexIDRename   map[string]string
	arcIDRename      map[string]string
	excludeRef       map[string]string
	dropProperty     map[string]map[string]struct{}
	excludeDangling  map[string]bool
	excludePair      map[string]bool
	arcPairSplit     map[string]string
	cast             map[kindProp]castRule
}

type kindProp struct{ Kind, Property string }

type castRule struct {
	target graph.Kind
	note   string
}

// ValidateDecisions checks the digest against the profile and that every
// action names an op the builder can apply.
func ValidateDecisions(p *profile.Profile, d *Decisions) error {
	if d.ProfileDigest != p.Digest {
		return fmt.Errorf("decisions digest %q does not match profile %q; rerun infer or restore the matching decisions file",
			d.ProfileDigest, p.Digest)
	}
	if !graph.ValidID(d.GraphName) {
		return fmt.Errorf("graph_name %q is not a valid identifier", d.GraphName)
	}
	for i, act := range d.Actions {
		if _, ok := allowedOps[act.Op]; !ok {
			return fmt.Errorf("action %d has unknown op %q", i, act.Op)
		}
	}
	return nil
}

var allowedOps = map[string]bool{
	"rename_vertex": true, "rename_arc": true,
	"rename_vertex_kind": true, "rename_arc_kind": true,
	"rename_vertex_property": true, "rename_arc_property": true,
	"split_vertex_property": true, "split_arc_property": true,
	"split_vertex_kind": true, "split_arc_kind_by_property": true,
	"split_arc_kind": true, "unify_endpoint_kinds": true,
	"exclude_vertex": true, "exclude_arc": true,
	"exclude_arc_pair": true, "exclude_dangling_arc": true,
	"drop_property": true, "explicit_cast": true,
}

// Build applies decisions to the inputs and assembles a validated graph.
// The returned graph has passed catalog.Validate and image size checks.
func Build(p *profile.Profile, d *Decisions, files []ingest.InputFile) (*graph.Graph, *Report, error) {
	if err := ValidateDecisions(p, d); err != nil {
		return nil, nil, err
	}
	idx, err := compile(d)
	if err != nil {
		return nil, nil, err
	}
	report := &Report{GraphName: d.GraphName, ProfileDigest: p.Digest}
	g, err := assembleGraph(d.GraphName, idx, files, report)
	if err != nil {
		return nil, nil, err
	}
	applyRuleFlags(g, d)
	report.Schema = g.Schema
	if err := catalogValidate(g); err != nil {
		return nil, nil, fmt.Errorf("assembled graph failed catalog validation: %w", err)
	}
	report.CatalogCheck = "passed"
	return g, report, nil
}

func compile(d *Decisions) (*index, error) {
	idx := &index{
		vertexKindRename: map[string]string{},
		arcKindRename:    map[string]string{},
		vertexPropRename: map[kindProp]string{},
		arcPropRename:    map[kindProp]string{},
		vertexPropSplit:  map[kindProp]map[graph.Kind]string{},
		arcPropSplit:     map[kindProp]map[graph.Kind]string{},
		vertexKindSplit:  map[kindProp]string{},
		vertexIDRename:   map[string]string{},
		arcIDRename:      map[string]string{},
		excludeRef:       map[string]string{},
		dropProperty:     map[string]map[string]struct{}{},
		excludeDangling:  map[string]bool{},
		excludePair:      map[string]bool{},
		arcPairSplit:     map[string]string{},
		cast:             map[kindProp]castRule{},
	}
	for _, act := range d.Actions {
		if err := idx.apply(act); err != nil {
			return nil, err
		}
	}
	return idx, nil
}

func (i *index) apply(act Action) error {
	ref := func() (string, error) {
		if act.ScopeRef == "" {
			return "", fmt.Errorf("action %s requires scope_ref", act.Op)
		}
		return act.ScopeRef, nil
	}
	switch act.Op {
	case "rename_vertex":
		r, err := ref()
		if err != nil {
			return err
		}
		i.vertexIDRename[r] = act.NewName
	case "rename_arc":
		r, err := ref()
		if err != nil {
			return err
		}
		i.arcIDRename[r] = act.NewName
	case "rename_vertex_kind":
		i.vertexKindRename[act.EntityKind] = act.NewName
	case "rename_arc_kind":
		i.arcKindRename[act.ArcKind] = act.NewName
	case "rename_vertex_property":
		i.vertexPropRename[kindProp{act.EntityKind, act.Property}] = act.NewName
	case "rename_arc_property":
		i.arcPropRename[kindProp{act.ArcKind, act.Property}] = act.NewName
	case "split_vertex_property":
		i.addSplit(i.vertexPropSplit, act.EntityKind, act.Property, act)
	case "split_arc_property":
		i.addSplit(i.arcPropSplit, act.ArcKind, act.Property, act)
	case "split_vertex_kind":
		i.vertexKindSplit[kindProp{act.EntityKind, act.Property}] = act.Suffix
	case "split_arc_kind_by_property":
		// Arc kinds cannot split on a property the way vertex kinds do;
		// routing is by endpoint pair instead.
		return fmt.Errorf("split_arc_kind_by_property is reported for review; use split_arc_kind with from/to routing")
	case "split_arc_kind":
		if act.From == "" || act.To == "" || act.NewName == "" {
			return fmt.Errorf("split_arc_kind requires from, to and new_name")
		}
		// Compiled lazily during arc routing; keep a marker via cost map
		// is inappropriate, so store in arcKindRename with pair key.
		i.arcPairSplit[pairKey(act.ArcKind, act.From, act.To)] = act.NewName
	case "unify_endpoint_kinds":
		return fmt.Errorf("unify_endpoint_kinds needs concrete rename_vertex_kind actions; add the confirmed mapping")
	case "exclude_vertex", "exclude_arc":
		r, err := ref()
		if err != nil {
			return err
		}
		i.excludeRef[r] = act.Op
	case "exclude_dangling_arc":
		i.excludeDangling[act.ArcKind] = true
	case "exclude_arc_pair":
		i.excludePair[pairKey(act.ArcKind, act.From, act.To)] = true
	case "drop_property":
		r, err := ref()
		if err != nil {
			return err
		}
		props := i.dropProperty[r]
		if props == nil {
			props = map[string]struct{}{}
			i.dropProperty[r] = props
		}
		props[act.Property] = struct{}{}
	case "explicit_cast":
		target := graph.Kind(act.NewName)
		if !target.Valid() {
			return fmt.Errorf("explicit_cast requires a valid target kind in new_name")
		}
		if strings.TrimSpace(act.OwnerNote) == "" {
			return fmt.Errorf("explicit_cast on %s/%s requires an owner_note authorizing the conversion",
				firstNonEmpty(act.EntityKind, act.ArcKind), act.Property)
		}
		i.cast[kindProp{firstNonEmpty(act.EntityKind, act.ArcKind), act.Property}] = castRule{target, act.OwnerNote}
	default:
		return fmt.Errorf("op %q is not implemented in the builder", act.Op)
	}
	return nil
}

func (i *index) addSplit(store map[kindProp]map[graph.Kind]string, kind, property string, act Action) {
	key := kindProp{kind, property}
	routes := store[key]
	if routes == nil {
		routes = map[graph.Kind]string{}
		store[key] = routes
	}
	routes[graph.Kind(act.ObservedKind)] = property + act.Suffix
}

func pairKey(kind, from, to string) string { return kind + "|" + from + "|" + to }

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// sortedKeys keeps every emitted artifact deterministic.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
