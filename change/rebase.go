package change

import (
	"maps"

	"rillweave/catalog"
	"rillweave/graph"
)

// Rebase replays an opt-in batch whose base revision is older than the current
// head. It returns a validated graph one revision ahead of head only when the
// intervening head changes can be proven independent of the batch; every
// undecidable or genuinely conflicting case returns graph.RebaseConflict or
// graph.RebaseUnavailable instead of silently overwriting the head.
//
// base is a retained immutable snapshot at batch.Base; head is the current
// immutable head. Retaining those snapshots is the engine's responsibility.
func Rebase(base, head *graph.Graph, batch Batch) (*graph.Graph, error) {
	if base.Revision != batch.Base {
		return nil, graph.Fail(graph.RebaseUnavailable,
			"revision %d is outside the bounded rebase window", batch.Base)
	}
	if err := checkBatch(head, batch); err != nil {
		return nil, err
	}
	// The batch must first be a valid transaction on its own base revision.
	candidate, err := stageChecked(base, batch)
	if err != nil {
		return nil, err
	}
	if err := catalog.Validate(candidate); err != nil {
		return nil, err
	}
	footprint := editFootprint(base, batch)
	if err := assertRebasable(base, head, candidate, footprint); err != nil {
		return nil, err
	}
	// Replay onto the current head and re-validate the merged final state, so a
	// constraint that the structural checks could not name is never published.
	merged := stage(head, batch)
	if err := catalog.Validate(merged); err != nil {
		return nil, graph.Fail(graph.RebaseConflict,
			"rebase would produce an invalid graph: %v", err)
	}
	merged.Revision++
	return merged, nil
}

type footprint struct {
	vertices      map[string]struct{}
	arcs          map[string]struct{}
	vertexKinds   map[string]struct{}
	arcKinds      map[string]struct{}
	schemaTouched bool
}

// editFootprint names every entity and rule kind the batch depends on, using
// the base revision to resolve kinds behind drop operations.
func editFootprint(base *graph.Graph, batch Batch) footprint {
	f := footprint{
		vertices:    map[string]struct{}{},
		arcs:        map[string]struct{}{},
		vertexKinds: map[string]struct{}{},
		arcKinds:    map[string]struct{}{},
	}
	for _, edit := range batch.Edits {
		switch edit.Op {
		case "put_vertex":
			if edit.Vertex != nil {
				f.vertices[edit.Vertex.ID] = struct{}{}
				f.vertexKinds[edit.Vertex.Kind] = struct{}{}
			}
		case "drop_vertex":
			if edit.ID != nil {
				f.vertices[*edit.ID] = struct{}{}
				if v, ok := base.Vertices[*edit.ID]; ok {
					f.vertexKinds[v.Kind] = struct{}{}
				}
			}
		case "put_arc":
			if edit.Arc != nil {
				f.arcs[edit.Arc.ID] = struct{}{}
				f.arcKinds[edit.Arc.Kind] = struct{}{}
				if rule, ok := base.Schema.Arcs[edit.Arc.Kind]; ok {
					f.vertexKinds[rule.From] = struct{}{}
					f.vertexKinds[rule.To] = struct{}{}
				}
			}
		case "drop_arc":
			if edit.ID != nil {
				f.arcs[*edit.ID] = struct{}{}
				if a, ok := base.Arcs[*edit.ID]; ok {
					f.arcKinds[a.Kind] = struct{}{}
					if rule, ok := base.Schema.Arcs[a.Kind]; ok {
						f.vertexKinds[rule.From] = struct{}{}
						f.vertexKinds[rule.To] = struct{}{}
					}
				}
			}
		case "set_schema":
			f.schemaTouched = true
		}
	}
	return f
}

func assertRebasable(base, head, candidate *graph.Graph, f footprint) error {
	// Only a schema change already committed on the head can cross this batch;
	// a batch-local set_schema against an unchanged head is handled entirely by
	// the final validation. When the head did change rules, the interaction is
	// manual if either side edits the schema or the change touches a kind the
	// batch uses (including the endpoint kinds behind its arc rules).
	if !sameSchema(base.Schema, head.Schema) && rulesTouched(base, head, f) {
		return graph.Fail(graph.RebaseConflict,
			"schema or endpoint rules changed for a kind used by this batch")
	}
	if err := overlapVertices(base, head, f.vertices); err != nil {
		return err
	}
	if err := overlapArcs(base, head, f.arcs); err != nil {
		return err
	}
	if err := endpointsIntact(base, head, f); err != nil {
		return err
	}
	if err := uniqueContention(head, candidate); err != nil {
		return err
	}
	return nil
}

// overlapVertices rejects a batch entity key whose record differs between the
// base revision and the head. A key the head changed and then restored to its
// base value is identical across both revisions, so it falls through as safe.
func overlapVertices(base, head *graph.Graph, keys map[string]struct{}) error {
	for id := range keys {
		baseValue, baseHad := base.Vertices[id]
		headValue, headHas := head.Vertices[id]
		if baseHad != headHas || (baseHad && !sameVertex(baseValue, headValue)) {
			return graph.Fail(graph.RebaseConflict,
				"vertex %q was changed by a committed revision and by this batch", id)
		}
	}
	return nil
}

func overlapArcs(base, head *graph.Graph, keys map[string]struct{}) error {
	for id := range keys {
		baseValue, baseHad := base.Arcs[id]
		headValue, headHas := head.Arcs[id]
		if baseHad != headHas || (baseHad && !sameArc(baseValue, headValue)) {
			return graph.Fail(graph.RebaseConflict,
				"arc %q was changed by a committed revision and by this batch", id)
		}
	}
	return nil
}

// rulesTouched reports whether the head's net schema change alters a vertex or
// arc kind (including endpoint kinds) that the batch uses, or whether the batch
// itself rewrites the catalog alongside an already-changed head catalog.
func rulesTouched(base, head *graph.Graph, f footprint) bool {
	if f.schemaTouched {
		return true
	}
	for kind := range f.vertexKinds {
		b, inBase := base.Schema.Vertices[kind]
		h, inHead := head.Schema.Vertices[kind]
		if inBase != inHead || !maps.Equal(b.Properties, h.Properties) {
			return true
		}
	}
	for kind := range f.arcKinds {
		b, inBase := base.Schema.Arcs[kind]
		h, inHead := head.Schema.Arcs[kind]
		if inBase != inHead || b.From != h.From || b.To != h.To ||
			!maps.Equal(b.Properties, h.Properties) {
			return true
		}
	}
	return false
}

// endpointsIntact catches references that cross the two write sets: a batch arc
// pointing at a vertex the head removed, or a batch vertex deletion whose
// arcs the head still relies on. Same-set interactions are already validated.
func endpointsIntact(base, head *graph.Graph, f footprint) error {
	for id := range f.arcs {
		a, ok := base.Arcs[id]
		if !ok {
			continue
		}
		if _, exists := head.Vertices[a.From]; !exists {
			return graph.Fail(graph.RebaseConflict,
				"arc %q loses source vertex %q to a committed revision", id, a.From)
		}
		if _, exists := head.Vertices[a.To]; !exists {
			return graph.Fail(graph.RebaseConflict,
				"arc %q loses target vertex %q to a committed revision", id, a.To)
		}
	}
	for id := range f.vertices {
		baseVertex, baseHad := base.Vertices[id]
		if !baseHad {
			continue
		}
		if _, stillThere := head.Vertices[id]; stillThere {
			continue
		}
		for arcID, a := range head.Arcs {
			if (a.From == id || a.To == id) && !arcInBatch(f, arcID) {
				return graph.Fail(graph.RebaseConflict,
					"dropped vertex %q still anchors committed arc %q", baseVertex.ID, arcID)
			}
		}
	}
	return nil
}

func arcInBatch(f footprint, id string) bool {
	_, ok := f.arcs[id]
	return ok
}

type uniqueClaim struct {
	kind, property string
	value          graph.Scalar
}

// uniqueClaims indexes every unique value an owner claims in a revision.
// catalog.Validate already guarantees each claim has at most one owner there.
func uniqueClaims(g *graph.Graph) map[uniqueClaim]string {
	claims := make(map[uniqueClaim]string)
	for _, id := range graph.Keys(g.Vertices) {
		v := g.Vertices[id]
		rule, ok := g.Schema.Vertices[v.Kind]
		if !ok {
			continue
		}
		for _, field := range graph.Keys(v.Properties) {
			if !rule.Properties[field].Unique {
				continue
			}
			claims[uniqueClaim{v.Kind, field, v.Properties[field]}] = id
		}
	}
	return claims
}

// uniqueContention rejects a value claimed by different owners on the two
// branches. Two different vertex keys fighting over one unique value can never
// be reconciled automatically even though each branch validated on its own.
func uniqueContention(head, candidate *graph.Graph) error {
	headClaims := uniqueClaims(head)
	candidateClaims := uniqueClaims(candidate)
	for claim, headOwner := range headClaims {
		candidateOwner, wanted := candidateClaims[claim]
		if wanted && candidateOwner != headOwner {
			return graph.Fail(graph.RebaseConflict,
				"unique %s value on kind %q is claimed by vertices %q and %q",
				claim.property, claim.kind, headOwner, candidateOwner)
		}
	}
	return nil
}
