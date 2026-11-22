package profile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"

	"rillweave/graph"
	"rillweave/ingest"
)

// crossChecks runs the checks that need complete scan state: conflicting
// scalar identities, arc endpoint compatibility, dangling endpoints and
// catalog capacity.
func (a *acc) crossChecks() {
	a.checkTypeConflicts(a.vObs, true)
	a.checkTypeConflicts(a.aObs, false)
	a.checkArcEndpoints()
	a.checkDangling()
	a.checkKindIdentifiers()
	a.checkCapacity()
}

// checkTypeConflicts turns multi-kind observations into split findings.
// Each plan below can be copied into the decisions document as-is.
func (a *acc) checkTypeConflicts(store map[string]map[string]map[graph.Kind]*typeObs, vertex bool) {
	entity := "vertex_kind"
	code := VertexTypeConflict
	if !vertex {
		entity = "arc_kind"
		code = ArcTypeConflict
	}
	for _, kind := range sortedKeys(store) {
		props := store[kind]
		for _, property := range sortedKeys(props) {
			byKind := props[property]
			if len(byKind) <= 1 {
				continue
			}
			subject := fmt.Sprintf("%s:%s/property:%s", entity, kind, property)
			observed := sortedKinds(byKind)
			var splitOp string
			if vertex {
				splitOp = "split_vertex_property"
			} else {
				splitOp = "split_arc_property"
			}
			var splitActions []PlanAction
			for _, k := range observed {
				splitActions = append(splitActions, PlanAction{
					Op: splitOp, EntityKind: ternary(vertex, kind, ""),
					ArcKind: ternary(!vertex, kind, ""), Property: property,
					ObservedKind: string(k), Suffix: "_" + string(k),
				})
			}
			f := a.addFinding(Finding{
				Code: code, Severity: SeverityConflict, Subject: subject,
				Message: fmt.Sprintf("property %q on %s %q is observed with %d distinct scalar identities %v; absent records are not a type",
					property, entity, kind, len(byKind), observed),
				Evidence: a.typeEvidence(vertex, kind, property),
				Plans: []Plan{{
					ID: "split-property", Label: "Split into one property per observed type",
					Description: "Rename to property_text / property_int / property_bool per record; no scalar value is rewritten.",
					Actions:     splitActions,
				}, {
					ID: "split-entity-kind", Label: "Split the entity kind by observed type",
					Description: "Records carrying different scalar identities become separate kinds; naming is confirmed later without rescanning.",
					Actions: []PlanAction{{Op: ternary(vertex, "split_vertex_kind", "split_arc_kind_by_property"),
						EntityKind: ternary(vertex, kind, ""), ArcKind: ternary(!vertex, kind, ""),
						Property: property, Suffix: "_by_" + property}},
				}, {
					ID: "explicit-cast", Label: "Owner-authorized explicit conversion (opt-in only)",
					Description: "Rillweave never coerces implicitly. This plan records the original token and requires a written owner note naming the target kind before build accepts it.",
					Actions: []PlanAction{{Op: "explicit_cast", EntityKind: ternary(vertex, kind, ""),
						ArcKind: ternary(!vertex, kind, ""), Property: property,
						NewName: string(observed[0]), RequireOwnerNote: true}},
				}},
			})
			a.attachPropertyFinding(vertex, kind, property, f.ID)
		}
	}
}

// typeEvidence collects one representative raw token per observed kind,
// keeping the original lexical form so text and integer stay visible.
func (a *acc) typeEvidence(vertex bool, kind, property string) []ingest.Evidence {
	out := make([]ingest.Evidence, 0, 3)
	seen := map[graph.Kind]bool{}
	for _, ev := range a.evidenceScan(vertex, kind, property) {
		k, _, problem := ingest.Classify([]byte(ev.Raw))
		if problem != "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, ev)
		if len(out) == 3 {
			return out
		}
	}
	return out
}

// checkArcEndpoints flags an arc kind connecting more than one endpoint
// pair. The catalog allows exactly one from/to rule per arc kind. Pair
// kinds are re-resolved here because arcs can precede their endpoints in
// the export order.
func (a *acc) checkArcEndpoints() {
	for _, kind := range sortedArcKeys(a.arcKinds) {
		ap := a.arcKinds[kind]
		resolved := map[string]*EndpointPair{}
		var ordered []string
		for _, rec := range a.arcRecords {
			if rec.raw.Kind != kind {
				continue
			}
			fromKind := a.kindByVertex[rec.raw.From]
			if fromKind == "" {
				fromKind = "unknown"
			}
			toKind := a.kindByVertex[rec.raw.To]
			if toKind == "" {
				toKind = "unknown"
			}
			key := fromKind + "|" + toKind
			pair, ok := resolved[key]
			if !ok {
				pair = &EndpointPair{From: fromKind, To: toKind}
				resolved[key] = pair
				ordered = append(ordered, key)
			}
			pair.Count++
			if len(pair.Arcs) < maxEvidence {
				pair.Arcs = append(pair.Arcs, rec.raw.ID)
			}
		}
		ap.Pairs = ap.Pairs[:0]
		for _, key := range ordered {
			ap.Pairs = append(ap.Pairs, *resolved[key])
		}
		sort.Slice(ap.Pairs, func(i, j int) bool {
			if ap.Pairs[i].From != ap.Pairs[j].From {
				return ap.Pairs[i].From < ap.Pairs[j].From
			}
			return ap.Pairs[i].To < ap.Pairs[j].To
		})
		if len(ap.Pairs) <= 1 {
			continue
		}
		var splitActions []PlanAction
		for _, pair := range ap.Pairs {
			splitActions = append(splitActions, PlanAction{
				Op: "split_arc_kind", ArcKind: kind, From: pair.From, To: pair.To,
				NewName: sanitize(kind + "_" + pair.From + "_" + pair.To),
			})
		}
		var evidence []ingest.Evidence
		for _, pair := range ap.Pairs {
			if len(pair.Arcs) > 0 {
				if ref, ok := a.arcIDs[pair.Arcs[0]]; ok {
					evidence = append(evidence, ingest.Evidence{Ref: ref})
				} else {
					evidence = append(evidence, ingest.Evidence{Ref: ingest.Ref{ID: pair.Arcs[0]}})
				}
			}
		}
		f := a.addFinding(Finding{
			Code: ArcEndpointConflict, Severity: SeverityConflict,
			Subject:  "arc_kind:" + kind,
			Message:  fmt.Sprintf("arc kind %q connects %d distinct endpoint pairs; a catalog arc rule declares exactly one from/to", kind, len(ap.Pairs)),
			Evidence: evidence,
			Plans: []Plan{{
				ID: "split-arc-kind", Label: "Create one arc kind per endpoint pair",
				Description: "Each observed pair becomes its own rule; arcs route by the vertex kinds at their ends.",
				Actions:     splitActions,
			}, {
				ID: "unify-endpoint-kinds", Label: "Unify endpoint vertex kinds through renames",
				Description: "If the endpoint kinds are actually the same concept, merge the vertex kinds in decisions; arcs then share one rule. Confirm the rename mapping before building.",
				Actions:     []PlanAction{{Op: "unify_endpoint_kinds", ArcKind: kind}},
			}, {
				ID: "exclude-pairs", Label: "Exclude arcs outside the canonical pair",
				Description: "Keep the most frequent pair and drop the other arcs; evidence lists every affected record for review.",
				Actions: []PlanAction{{Op: "exclude_arc_pair", ArcKind: kind,
					From: ap.Pairs[0].From, To: ap.Pairs[0].To}},
			}},
		})
		ap.FindingIDs = append(ap.FindingIDs, f.ID)
	}
}

// checkDangling reports arcs whose endpoints never appear as vertices.
func (a *acc) checkDangling() {
	for _, kind := range sortedArcKeys(a.arcKinds) {
		ap := a.arcKinds[kind]
		var evidence []ingest.Evidence
		for _, ref := range a.arcRefs(kind) {
			arc := a.rawArc(ref)
			_, fromOK := a.kindByVertex[arc.From]
			_, toOK := a.kindByVertex[arc.To]
			if !fromOK || !toOK {
				evidence = append(evidence, ingest.Evidence{Ref: ref})
			}
		}
		if len(evidence) == 0 {
			continue
		}
		f := a.addFinding(Finding{
			Code: DanglingArc, Severity: SeverityBlocker,
			Subject:  "arc_kind:" + kind,
			Message:  fmt.Sprintf("arc kind %q has %d arcs with missing endpoints; catalog validation requires both endpoints present", kind, len(evidence)),
			Evidence: evidence,
			Plans: []Plan{{
				ID: "exclude-dangling", Label: "Exclude dangling arcs",
				Description: "Omit arcs whose endpoint ids are absent from the vertex set.",
				Actions:     []PlanAction{{Op: "exclude_dangling_arc", ArcKind: kind}},
			}},
		})
		ap.FindingIDs = append(ap.FindingIDs, f.ID)
	}
}

// checkKindIdentifiers catches invalid kind names even when every id is
// valid; kinds themselves must satisfy the same identifier grammar.
func (a *acc) checkKindIdentifiers() {
	for _, kind := range sortedKeys(a.vertexKinds) {
		if graph.ValidID(kind) {
			continue
		}
		f := a.addFinding(Finding{
			Code: InvalidIdentifier, Severity: SeverityBlocker,
			Subject:  "vertex_kind:" + kind,
			Message:  fmt.Sprintf("vertex kind %q is not a valid identifier", kind),
			Evidence: []ingest.Evidence{},
			Plans: []Plan{{
				ID: "sanitize-kind", Label: "Sanitize the kind name",
				Description: "Rewrite the kind through the identifier sanitizer; all vertices of this kind move together.",
				Actions:     []PlanAction{{Op: "rename_vertex_kind", EntityKind: kind, NewName: sanitize(kind)}},
			}},
		})
		a.vertexKinds[kind].FindingIDs = append(a.vertexKinds[kind].FindingIDs, f.ID)
	}
	for _, kind := range sortedArcKeys(a.arcKinds) {
		if graph.ValidID(kind) {
			continue
		}
		f := a.addFinding(Finding{
			Code: InvalidIdentifier, Severity: SeverityBlocker,
			Subject:  "arc_kind:" + kind,
			Message:  fmt.Sprintf("arc kind %q is not a valid identifier", kind),
			Evidence: []ingest.Evidence{},
			Plans: []Plan{{
				ID: "sanitize-kind", Label: "Sanitize the kind name",
				Description: "Rewrite the kind through the identifier sanitizer; all arcs of this kind move together.",
				Actions:     []PlanAction{{Op: "rename_arc_kind", ArcKind: kind, NewName: sanitize(kind)}},
			}},
		})
		a.arcKinds[kind].FindingIDs = append(a.arcKinds[kind].FindingIDs, f.ID)
	}
}

func (a *acc) checkCapacity() {
	var violations []string
	if a.totalVertices > graph.MaxVertices {
		violations = append(violations, fmt.Sprintf("%d vertices exceeds %d", a.totalVertices, graph.MaxVertices))
	}
	if a.totalArcs > graph.MaxArcs {
		violations = append(violations, fmt.Sprintf("%d arcs exceeds %d", a.totalArcs, graph.MaxArcs))
	}
	if len(a.vertexKinds) > graph.MaxRules || len(a.arcKinds) > graph.MaxRules {
		violations = append(violations, fmt.Sprintf("kind counts %d/%d exceed %d rules each", len(a.vertexKinds), len(a.arcKinds), graph.MaxRules))
	}
	if len(violations) == 0 {
		return
	}
	a.addFinding(Finding{
		Code: CapacityExceeded, Severity: SeverityAdvisory, Subject: "catalog",
		Message:  fmt.Sprintf("catalog capacity pre-check failed: %v", violations),
		Evidence: []ingest.Evidence{},
	})
}

// finalize converts accumulators into the stable artifact and computes
// its digest over the canonical input set plus every finding.
func (a *acc) finalize() {
	a.materializeProperties()
	a.p.Findings = a.findings
	for _, f := range a.findings {
		switch f.Severity {
		case SeverityBlocker:
			a.p.Counts.Blockers++
		case SeverityConflict:
			a.p.Counts.Conflicts++
		default:
			a.p.Counts.Advisories++
		}
	}
	a.p.Counts.UniqueVertices = len(a.vertexIDs)
	a.p.Counts.UniqueArcs = len(a.arcIDs)
	for _, ap := range a.arcKinds {
		sort.Slice(ap.Pairs, func(i, j int) bool {
			if ap.Pairs[i].From != ap.Pairs[j].From {
				return ap.Pairs[i].From < ap.Pairs[j].From
			}
			return ap.Pairs[i].To < ap.Pairs[j].To
		})
	}
	a.p.Digest = digestProfile(a.p)
}

func (a *acc) materializeProperties() {
	for kind, props := range a.vObs {
		vp := a.vertexKinds[kind]
		for name, byKind := range props {
			pp := vp.Properties[name]
			if pp == nil {
				pp = &PropertyProfile{}
				vp.Properties[name] = pp
			}
			fillProfile(pp, byKind, vp.Count)
		}
		// Record missing counts for every observed property.
		for _, pp := range vp.Properties {
			if pp.Present == 0 {
				continue
			}
			pp.Missing = vp.Count - pp.Present
		}
	}
	for kind, props := range a.aObs {
		ap := a.arcKinds[kind]
		for name, byKind := range props {
			pp := ap.Properties[name]
			if pp == nil {
				pp = &PropertyProfile{}
				ap.Properties[name] = pp
			}
			fillProfile(pp, byKind, ap.Count)
			pp.Missing = ap.Count - pp.Present
		}
	}
}

func fillProfile(pp *PropertyProfile, byKind map[graph.Kind]*typeObs, total int) {
	for _, k := range sortedKinds(byKind) {
		o := byKind[k]
		pp.Types = append(pp.Types, Observation{
			Kind: k, Count: o.count, Unique: len(o.values),
		})
		pp.Present += o.count
	}
	if pp.Missing == 0 {
		pp.Missing = total - pp.Present
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

var sortedArcKeys = sortedKeys[*ArcProfile]

func sortedKinds(m map[graph.Kind]*typeObs) []graph.Kind {
	keys := make([]graph.Kind, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

// sanitize rewrites an external name into Rillweave identifier grammar:
// lowercase ASCII, first byte alphanumeric, remaining bytes from
// [a-z0-9_-]. It is deterministic so profile and build agree.
func sanitize(name string) string {
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

// digestProfile hashes the stable input checksums plus finding ids and
// subjects. Build recomputes it and refuses stale decisions.
func digestProfile(p *Profile) string {
	h := sha256.New()
	for _, in := range p.InputDigest {
		fmt.Fprintf(h, "%s|%s\n", in.Path, in.SHA256)
	}
	for _, f := range p.Findings {
		fmt.Fprintf(h, "%s|%s|%s\n", f.ID, f.Code, f.Subject)
	}
	return hex.EncodeToString(h.Sum(nil))
}
