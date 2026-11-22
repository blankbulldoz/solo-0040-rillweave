// Package profile derives a type-level view of external graph exports.
// It scans every record, keeps observed scalar identities distinct, and
// reports every conflict with record-level evidence plus concrete split
// plans. Inference and decision application are separate phases so that
// renaming confirmed by the data owner never forces a full re-scan.
package profile

import (
	"fmt"
	"os"

	"rillweave/graph"
	"rillweave/ingest"
)

// Finding categories. Blockers stop an unconfirmed build; conflicts carry
// split plans; advisories describe catalog rules the owner must approve.
const (
	VertexTypeConflict  = "vertex_property_type_conflict"
	ArcTypeConflict     = "arc_property_type_conflict"
	ArcEndpointConflict = "arc_endpoint_pair_conflict"
	UnsupportedScalar   = "unsupported_scalar"
	InvalidIdentifier   = "invalid_identifier"
	DuplicateVertex     = "duplicate_vertex_id"
	DuplicateArc        = "duplicate_arc_id"
	DanglingArc         = "dangling_arc_endpoint"
	VertexKindMismatch  = "vertex_kind_mismatch"
	InvalidArcCost      = "invalid_arc_cost"
	CapacityExceeded    = "capacity_exceeded"
)

// Severity orders report output; every category is either blocking or a
// conflict that must be resolved before a graph can pass catalog checks.
const (
	SeverityBlocker  = "blocker"
	SeverityConflict = "conflict"
	SeverityAdvisory = "advisory"
)

const maxEvidence = 20

// Observation counts one scalar kind for one property on one entity kind.
type Observation struct {
	Kind   graph.Kind `json:"kind"`
	Count  int        `json:"count"`
	Unique int        `json:"unique_values"`
}

// PropertyProfile is the inferred shape of one property name, including
// how often it is absent. Missing values are not counted as any type.
type PropertyProfile struct {
	Present    int           `json:"present"`
	Missing    int           `json:"missing"`
	Types      []Observation `json:"types"`
	FindingIDs []string      `json:"finding_ids,omitempty"`
}

// EndpointPair records one observed from-kind/to-kind combination for an
// arc kind and the arc IDs that realize it.
type EndpointPair struct {
	From  string   `json:"from"`
	To    string   `json:"to"`
	Count int      `json:"count"`
	Arcs  []string `json:"arcs"`
}

// ArcProfile covers endpoint compatibility and arc-level properties.
type ArcProfile struct {
	Count      int                         `json:"count"`
	Pairs      []EndpointPair              `json:"pairs"`
	Properties map[string]*PropertyProfile `json:"properties,omitempty"`
	FindingIDs []string                    `json:"finding_ids,omitempty"`
}

// VertexProfile covers one vertex kind.
type VertexProfile struct {
	Count      int                         `json:"count"`
	Properties map[string]*PropertyProfile `json:"properties,omitempty"`
	FindingIDs []string                    `json:"finding_ids,omitempty"`
}

// Plan is one applicable resolution for a finding. Actions use the exact
// schema understood by assemble so a confirmed plan can be copied into
// the decisions document without translation.
type Plan struct {
	ID          string       `json:"id"`
	Label       string       `json:"label"`
	Description string       `json:"description"`
	Actions     []PlanAction `json:"actions"`
}

// PlanAction mirrors assemble.Action but keeps plan-specific routing
// (for example per-type suffixes) inside named fields.
type PlanAction struct {
	Op               string `json:"op"`
	EntityKind       string `json:"entity_kind,omitempty"`
	Property         string `json:"property,omitempty"`
	ObservedKind     string `json:"observed_kind,omitempty"`
	Suffix           string `json:"suffix,omitempty"`
	NewName          string `json:"new_name,omitempty"`
	ArcKind          string `json:"arc_kind,omitempty"`
	From             string `json:"from,omitempty"`
	To               string `json:"to,omitempty"`
	ScopeRef         string `json:"scope_ref,omitempty"`
	RequireOwnerNote bool   `json:"require_owner_note,omitempty"`
}

// Finding is a typed conflict or rule violation with traceable evidence.
type Finding struct {
	ID        string            `json:"id"`
	Code      string            `json:"code"`
	Severity  string            `json:"severity"`
	Subject   string            `json:"subject"`
	Message   string            `json:"message"`
	Evidence  []ingest.Evidence `json:"evidence"`
	Truncated bool              `json:"evidence_truncated"`
	Plans     []Plan            `json:"plans,omitempty"`
}

// InputDigest identifies one input file inside a profile.
type InputDigest struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// Profile is the complete inference artifact.
type Profile struct {
	Version     int                       `json:"version"`
	InputDigest []InputDigest             `json:"input_digest"`
	Counts      Counts                    `json:"counts"`
	VertexKinds map[string]*VertexProfile `json:"vertex_kinds"`
	ArcKinds    map[string]*ArcProfile    `json:"arc_kinds"`
	Findings    []Finding                 `json:"findings"`
	Digest      string                    `json:"digest"`
}

// Counts summarizes scan volume for reviewer sanity checks.
type Counts struct {
	Files          int `json:"files"`
	VertexRecords  int `json:"vertex_records"`
	ArcRecords     int `json:"arc_records"`
	UniqueVertices int `json:"unique_vertex_ids"`
	UniqueArcs     int `json:"unique_arc_ids"`
	Blockers       int `json:"blockers"`
	Conflicts      int `json:"conflicts"`
	Advisories     int `json:"advisories"`
}

// Internal accumulator state.
type acc struct {
	p           *Profile
	vertexKinds map[string]*VertexProfile
	arcKinds    map[string]*ArcProfile
	// property type tracking
	vObs         map[string]map[string]map[graph.Kind]*typeObs
	aObs         map[string]map[string]map[graph.Kind]*typeObs
	vertexIDs    map[string]ingest.Ref
	arcIDs       map[string]ingest.Ref
	kindByVertex map[string]string
	findings     []Finding
	counter      int
	// retained decoded records for evidence-level conflict reporting
	vertexRecords []vertexRecord
	arcRecords    []arcRecord
	// capacity pre-checks
	totalVertices int
	totalArcs     int
}

type typeObs struct {
	count  int
	values map[string]struct{}
}

func newAcc() *acc {
	return &acc{
		vertexKinds:  map[string]*VertexProfile{},
		arcKinds:     map[string]*ArcProfile{},
		vObs:         map[string]map[string]map[graph.Kind]*typeObs{},
		aObs:         map[string]map[string]map[graph.Kind]*typeObs{},
		vertexIDs:    map[string]ingest.Ref{},
		arcIDs:       map[string]ingest.Ref{},
		kindByVertex: map[string]string{},
	}
}

// Build scans every input file completely and returns the profile.
func Build(files []ingest.InputFile) *Profile {
	a := newAcc()
	a.p = &Profile{Version: 1, VertexKinds: a.vertexKinds, ArcKinds: a.arcKinds}
	for _, f := range files {
		info, err := os.Stat(f.Path)
		if err != nil {
			// Checksummed inputs were already read; a stat failure here
			// means the directory changed during the scan.
			a.p.InputDigest = append(a.p.InputDigest, InputDigest{
				Path: f.Path, SHA256: f.SHA256,
			})
		} else {
			a.p.InputDigest = append(a.p.InputDigest, InputDigest{
				Path: f.Path, SHA256: f.SHA256, Bytes: info.Size(),
			})
		}
		a.scanFile(f)
	}
	a.crossChecks()
	a.finalize()
	return a.p
}

func (a *acc) addFinding(f Finding) Finding {
	a.counter++
	f.ID = fmt.Sprintf("F%03d", a.counter)
	if f.Evidence == nil {
		f.Evidence = []ingest.Evidence{}
	}
	if len(f.Evidence) > maxEvidence {
		f.Evidence = f.Evidence[:maxEvidence]
		f.Truncated = true
	}
	a.findings = append(a.findings, f)
	return f
}

func (a *acc) scanFile(f ingest.InputFile) {
	a.p.Counts.Files++
	a.attachRecords(f)
	for i, v := range f.Export.Vertices {
		a.p.Counts.VertexRecords++
		a.totalVertices++
		ref := ingest.Ref{File: f.Path, Ordinal: i, ID: v.ID}
		a.scanVertex(v, ref)
	}
	for i, arc := range f.Export.Arcs {
		a.p.Counts.ArcRecords++
		a.totalArcs++
		ref := ingest.Ref{File: f.Path, Ordinal: i, ID: arc.ID}
		a.scanArc(arc, ref)
	}
}

func (a *acc) scanVertex(v ingest.RawVertex, ref ingest.Ref) {
	vp, ok := a.vertexKinds[v.Kind]
	if !ok {
		vp = &VertexProfile{Properties: map[string]*PropertyProfile{}}
		a.vertexKinds[v.Kind] = vp
	}
	vp.Count++
	if prev, dup := a.vertexIDs[v.ID]; dup {
		f := a.addFinding(Finding{
			Code: DuplicateVertex, Severity: SeverityBlocker,
			Subject:  "vertex:" + v.ID,
			Message:  fmt.Sprintf("vertex id %q appears in %s and %s", v.ID, prev.File, ref.File),
			Evidence: []ingest.Evidence{{Ref: prev}, {Ref: ref}},
			Plans: []Plan{{
				ID: "rename-later-declaration", Label: "Rename the later duplicate vertex",
				Description: "The first declaration keeps the id; the later record receives a deterministic suffixed id by file and ordinal.",
				Actions: []PlanAction{{Op: "rename_vertex", EntityKind: v.Kind,
					ScopeRef: refFileOrdinal(ref), NewName: sanitize(v.ID + "_dup" + fmt.Sprintf("%d", ref.Ordinal))}},
			}, {
				ID: "exclude-later-declaration", Label: "Exclude the later duplicate vertex",
				Description: "Drop the later record and keep the first declaration as the single vertex with this id.",
				Actions:     []PlanAction{{Op: "exclude_vertex", EntityKind: v.Kind, ScopeRef: refFileOrdinal(ref)}},
			}},
		})
		vp.FindingIDs = append(vp.FindingIDs, f.ID)
	} else {
		a.vertexIDs[v.ID] = ref
	}
	if prevKind, seen := a.kindByVertex[v.ID]; seen && prevKind != v.Kind {
		f := a.addFinding(Finding{
			Code: VertexKindMismatch, Severity: SeverityBlocker,
			Subject:  "vertex:" + v.ID,
			Message:  fmt.Sprintf("vertex %q is declared as %q and %q", v.ID, prevKind, v.Kind),
			Evidence: []ingest.Evidence{{Ref: ref}},
			Plans: []Plan{{
				ID: "rename-later-declaration", Label: "Rename the conflicting vertex id",
				Description: "Assign a new id to the record outside the established kind; assemble applies the rename by file and ordinal.",
				Actions:     []PlanAction{{Op: "rename_vertex", EntityKind: v.Kind, ScopeRef: refFileOrdinal(ref), NewName: v.ID + "-" + v.Kind}},
			}},
		})
		vp.FindingIDs = append(vp.FindingIDs, f.ID)
	}
	a.kindByVertex[v.ID] = v.Kind
	if !graph.ValidID(v.ID) || !graph.ValidID(v.Kind) {
		f := a.addFinding(Finding{
			Code: InvalidIdentifier, Severity: SeverityBlocker,
			Subject:  "vertex:" + v.ID,
			Message:  fmt.Sprintf("vertex id or kind %q/%q is not a lowercase ASCII identifier", v.ID, v.Kind),
			Evidence: []ingest.Evidence{{Ref: ref}},
			Plans: []Plan{{
				ID: "sanitize-identifier", Label: "Sanitize to a valid identifier",
				Description: "Lower-case, replace non-identifier bytes with underscores and prefix a letter; collisions get numeric suffixes.",
				Actions:     []PlanAction{{Op: "rename_vertex", EntityKind: v.Kind, ScopeRef: refFileOrdinal(ref), NewName: sanitize(v.ID)}},
			}},
		})
		vp.FindingIDs = append(vp.FindingIDs, f.ID)
	}
	for name, raw := range v.Properties {
		if !graph.ValidID(name) {
			f := a.addFinding(Finding{
				Code: InvalidIdentifier, Severity: SeverityBlocker,
				Subject:  fmt.Sprintf("vertex_kind:%s/property:%s", v.Kind, name),
				Message:  fmt.Sprintf("property name %q on vertex kind %q is not a valid identifier", name, v.Kind),
				Evidence: []ingest.Evidence{ingest.EvidenceFor(ref, raw)},
				Plans: []Plan{{
					ID: "sanitize-property", Label: "Sanitize the property name",
					Description: "Rewrite the property name through the same identifier sanitizer across every kind that uses it.",
					Actions:     []PlanAction{{Op: "rename_vertex_property", EntityKind: v.Kind, Property: name, NewName: sanitize(name)}},
				}},
			})
			vp.FindingIDs = append(vp.FindingIDs, f.ID)
		}
		a.observe(a.vObs, v.Kind, name, raw, ref, true)
	}
}

func (a *acc) scanArc(arc ingest.RawArc, ref ingest.Ref) {
	ap, ok := a.arcKinds[arc.Kind]
	if !ok {
		ap = &ArcProfile{Properties: map[string]*PropertyProfile{}}
		a.arcKinds[arc.Kind] = ap
	}
	ap.Count++
	fromKind := a.kindByVertex[arc.From]
	if fromKind == "" {
		fromKind = "unknown"
	}
	toKind := a.kindByVertex[arc.To]
	if toKind == "" {
		toKind = "unknown"
	}
	pair := endpointPair(ap, fromKind, toKind)
	pair.Count++
	if len(pair.Arcs) < maxEvidence {
		pair.Arcs = append(pair.Arcs, arc.ID)
	}
	if prev, dup := a.arcIDs[arc.ID]; dup {
		f := a.addFinding(Finding{
			Code: DuplicateArc, Severity: SeverityBlocker,
			Subject:  "arc:" + arc.ID,
			Message:  fmt.Sprintf("arc id %q appears in %s and %s", arc.ID, prev.File, ref.File),
			Evidence: []ingest.Evidence{{Ref: prev}, {Ref: ref}},
			Plans: []Plan{{
				ID: "rename-later-declaration", Label: "Rename the later duplicate arc",
				Description: "The first declaration keeps the id; the later record receives a deterministic suffixed id by file and ordinal.",
				Actions: []PlanAction{{Op: "rename_arc", ArcKind: arc.Kind,
					ScopeRef: refFileOrdinal(ref), NewName: sanitize(arc.ID + "_dup" + fmt.Sprintf("%d", ref.Ordinal))}},
			}, {
				ID: "exclude-later-declaration", Label: "Exclude the later duplicate arc",
				Description: "Drop the later record and keep the first declaration as the single arc with this id.",
				Actions:     []PlanAction{{Op: "exclude_arc", ArcKind: arc.Kind, ScopeRef: refFileOrdinal(ref)}},
			}},
		})
		ap.FindingIDs = append(ap.FindingIDs, f.ID)
	} else {
		a.arcIDs[arc.ID] = ref
	}
	if !graph.ValidID(arc.ID) || !graph.ValidID(arc.Kind) {
		f := a.addFinding(Finding{
			Code: InvalidIdentifier, Severity: SeverityBlocker,
			Subject:  "arc:" + arc.ID,
			Message:  fmt.Sprintf("arc id or kind %q/%q is not a lowercase ASCII identifier", arc.ID, arc.Kind),
			Evidence: []ingest.Evidence{{Ref: ref}},
			Plans: []Plan{{
				ID: "sanitize-identifier", Label: "Sanitize to a valid identifier",
				Description: "Rewrite the arc id through the identifier sanitizer with collision suffixes.",
				Actions:     []PlanAction{{Op: "rename_arc", ArcKind: arc.Kind, ScopeRef: refFileOrdinal(ref), NewName: sanitize(arc.ID)}},
			}},
		})
		ap.FindingIDs = append(ap.FindingIDs, f.ID)
	}
	if len(arc.Cost) > 0 {
		kind, _, problem := ingest.Classify(arc.Cost)
		if problem != "" || kind != graph.Int {
			f := a.addFinding(Finding{
				Code: InvalidArcCost, Severity: SeverityBlocker,
				Subject:  "arc:" + arc.ID,
				Message:  fmt.Sprintf("arc %q cost must be a positive integer; observed problem %q", arc.ID, problemOrKind(problem, kind)),
				Evidence: []ingest.Evidence{ingest.EvidenceFor(ref, arc.Cost)},
				Plans: []Plan{{
					ID: "exclude-arc", Label: "Exclude this arc from the installable graph",
					Description: "Catalog costs are strictly positive integers; without an owner-supplied integer the record cannot be included.",
					Actions:     []PlanAction{{Op: "exclude_arc", ScopeRef: refFileOrdinal(ref)}},
				}},
			})
			ap.FindingIDs = append(ap.FindingIDs, f.ID)
		}
	} else {
		f := a.addFinding(Finding{
			Code: InvalidArcCost, Severity: SeverityBlocker,
			Subject:  "arc:" + arc.ID,
			Message:  fmt.Sprintf("arc %q has no cost; Rillweave requires a positive integer cost", arc.ID),
			Evidence: []ingest.Evidence{{Ref: ref}},
			Plans: []Plan{{
				ID: "exclude-arc", Label: "Exclude this arc from the installable graph",
				Description: "The owner can instead add an explicit cost override in decisions.",
				Actions:     []PlanAction{{Op: "exclude_arc", ScopeRef: refFileOrdinal(ref)}},
			}},
		})
		ap.FindingIDs = append(ap.FindingIDs, f.ID)
	}
	for name, raw := range arc.Properties {
		if !graph.ValidID(name) {
			f := a.addFinding(Finding{
				Code: InvalidIdentifier, Severity: SeverityBlocker,
				Subject:  fmt.Sprintf("arc_kind:%s/property:%s", arc.Kind, name),
				Message:  fmt.Sprintf("property name %q on arc kind %q is not a valid identifier", name, arc.Kind),
				Evidence: []ingest.Evidence{ingest.EvidenceFor(ref, raw)},
				Plans: []Plan{{
					ID: "sanitize-property", Label: "Sanitize the property name",
					Description: "Rewrite the property name through the identifier sanitizer.",
					Actions:     []PlanAction{{Op: "rename_arc_property", ArcKind: arc.Kind, Property: name, NewName: sanitize(name)}},
				}},
			})
			ap.FindingIDs = append(ap.FindingIDs, f.ID)
		}
		a.observe(a.aObs, arc.Kind, name, raw, ref, false)
	}
}

func problemOrKind(problem string, kind graph.Kind) string {
	if problem != "" {
		return problem
	}
	return "observed kind " + string(kind)
}

func endpointPair(ap *ArcProfile, from, to string) *EndpointPair {
	for i := range ap.Pairs {
		if ap.Pairs[i].From == from && ap.Pairs[i].To == to {
			return &ap.Pairs[i]
		}
	}
	ap.Pairs = append(ap.Pairs, EndpointPair{From: from, To: to})
	return &ap.Pairs[len(ap.Pairs)-1]
}

// observe classifies one raw property value. Unsupported tokens become
// findings; scalar kinds accumulate separately so no widening occurs.
func (a *acc) observe(store map[string]map[string]map[graph.Kind]*typeObs, kind, property string,
	raw []byte, ref ingest.Ref, vertex bool) {
	k, value, problem := ingest.Classify(raw)
	entity := "vertex_kind"
	if !vertex {
		entity = "arc_kind"
	}
	subject := fmt.Sprintf("%s:%s/property:%s", entity, kind, property)
	if problem != "" {
		ev := ingest.EvidenceFor(ref, raw)
		f := a.addFinding(Finding{
			Code: UnsupportedScalar, Severity: SeverityBlocker, Subject: subject,
			Message: fmt.Sprintf("property %q on %s %q has an unsupported value (%s); original token is preserved, no conversion applied",
				property, entity, kind, problem),
			Evidence: []ingest.Evidence{ev},
			Plans: []Plan{{
				ID: "drop-property-record", Label: "Drop this property on the offending record",
				Description: "Keep the entity but omit the unsupported property for this record only.",
				Actions:     []PlanAction{{Op: "drop_property", EntityKind: ternary(vertex, kind, ""), ArcKind: ternary(!vertex, kind, ""), Property: property, ScopeRef: refFileOrdinal(ref)}},
			}, {
				ID: "exclude-record", Label: "Exclude the offending record",
				Description: "Omit the whole vertex or arc from the installable graph.",
				Actions:     []PlanAction{{Op: ternary(vertex, "exclude_vertex", "exclude_arc"), EntityKind: ternary(vertex, kind, ""), ArcKind: ternary(!vertex, kind, ""), ScopeRef: refFileOrdinal(ref)}},
			}},
		})
		a.attachPropertyFinding(vertex, kind, property, f.ID)
		return
	}
	props := store[kind]
	if props == nil {
		props = map[string]map[graph.Kind]*typeObs{}
		store[kind] = props
	}
	byKind := props[property]
	if byKind == nil {
		byKind = map[graph.Kind]*typeObs{}
		props[property] = byKind
	}
	o := byKind[k]
	if o == nil {
		o = &typeObs{values: map[string]struct{}{}}
		byKind[k] = o
	}
	o.count++
	o.values[valueToken(k, value)] = struct{}{}
}

func ternary(cond bool, whenTrue, whenFalse string) string {
	if cond {
		return whenTrue
	}
	return whenFalse
}

func valueToken(k graph.Kind, v graph.Scalar) string {
	switch k {
	case graph.Text:
		return "t:" + v.Text
	case graph.Int:
		return fmt.Sprintf("i:%d", v.Int)
	default:
		return "b:" + fmt.Sprintf("%t", v.Bool)
	}
}

func (a *acc) attachPropertyFinding(vertex bool, kind, property, findingID string) {
	if vertex {
		if vp := a.vertexKinds[kind]; vp != nil {
			if vp.Properties[property] == nil {
				vp.Properties[property] = &PropertyProfile{}
			}
			vp.Properties[property].FindingIDs = append(vp.Properties[property].FindingIDs, findingID)
			vp.FindingIDs = append(vp.FindingIDs, findingID)
		}
		return
	}
	if ap := a.arcKinds[kind]; ap != nil {
		if ap.Properties[property] == nil {
			ap.Properties[property] = &PropertyProfile{}
		}
		ap.Properties[property].FindingIDs = append(ap.Properties[property].FindingIDs, findingID)
		ap.FindingIDs = append(ap.FindingIDs, findingID)
	}
}

func refFileOrdinal(ref ingest.Ref) string {
	return ref.File + "#" + fmt.Sprintf("%d", ref.Ordinal)
}
