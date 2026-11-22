package change

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"

	"rillweave/catalog"
	"rillweave/graph"
)

const MaxEdits = 2000

type Edit struct {
	Op      string        `json:"op"`
	ID      *string       `json:"id,omitempty"`
	Vertex  *graph.Vertex `json:"vertex,omitempty"`
	Arc     *graph.Arc    `json:"arc,omitempty"`
	Schema  *graph.Schema `json:"schema,omitempty"`
	Rewrite *Rewrite      `json:"rewrite,omitempty"`
}

// Confirm binds a commit to a preview. The preview base must equal the batch
// base and the digest must describe the same base revision and edit list, so a
// plan computed against an older state cannot be applied as if it were current.
type Confirm struct {
	Base   uint64 `json:"base"`
	Digest string `json:"digest"`
}

type Batch struct {
	Base    uint64   `json:"base"`
	Edits   []Edit   `json:"edits"`
	Confirm *Confirm `json:"confirm,omitempty"`
}

// Plan describes what a batch would do without publishing it. Its digest is
// the confirmation reference returned by a preview.
type Plan struct {
	Base         uint64          `json:"base"`
	BaseDigest   string          `json:"base_digest"`
	NextRevision uint64          `json:"next_revision"`
	Rewrites     []RewriteReport `json:"rewrites"`
	Digest       string          `json:"digest"`
}

func Prepare(head *graph.Graph, batch Batch) (*graph.Graph, error) {
	next, _, err := PreparePlan(head, batch)
	return next, err
}

// PreparePlan applies the batch to a private candidate and reports the rewrite
// plan. Nothing is published and the head is not modified.
func PreparePlan(head *graph.Graph, batch Batch) (*graph.Graph, Plan, error) {
	if batch.Base != head.Revision {
		return nil, Plan{}, graph.Fail(graph.Conflict, "base %d is not current revision %d", batch.Base, head.Revision)
	}
	if len(batch.Edits) == 0 || len(batch.Edits) > MaxEdits {
		return nil, Plan{}, graph.Fail(graph.Invalid, "a commit requires 1..%d edits", MaxEdits)
	}
	if head.Revision == math.MaxUint64 {
		return nil, Plan{}, graph.Fail(graph.Budget, "revision space exhausted")
	}
	if batch.Confirm != nil {
		if batch.Confirm.Base != batch.Base {
			return nil, Plan{}, graph.Fail(graph.Invalid,
				"confirmation base %d does not match batch base %d", batch.Confirm.Base, batch.Base)
		}
		expected := Digest(head, batch)
		if batch.Confirm.Digest != expected {
			return nil, Plan{}, graph.Fail(graph.Conflict,
				"confirmation refers to a preview that does not match the current head and edits; preview again")
		}
	}
	candidate := head.Clone()
	rewrites := []RewriteReport{}
	for n, edit := range batch.Edits {
		report, err := apply(candidate, edit)
		if err != nil {
			return nil, Plan{}, fmt.Errorf("edit %d: %w", n, err)
		}
		if report != nil {
			rewrites = append(rewrites, *report)
		}
	}
	if err := catalog.Validate(candidate); err != nil {
		return nil, Plan{}, err
	}
	candidate.Revision++
	plan := Plan{
		Base: head.Revision, BaseDigest: stateDigest(head),
		NextRevision: candidate.Revision, Rewrites: rewrites,
	}
	plan.Digest = Digest(head, batch)
	return candidate, plan, nil
}

// stateDigest identifies the head contents, not just its revision number, so
// two different graphs that share a revision do not share a confirmation.
func stateDigest(g *graph.Graph) string {
	raw, err := json.Marshal(g)
	if err != nil {
		return "invalid"
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Digest identifies the previewed state and edit list. It covers the base
// state and the full ordered edits, so an edited request or a head that changed
// under the preview invalidates a confirmation.
func Digest(head *graph.Graph, batch Batch) string {
	type reference struct {
		StateDigest string `json:"state_digest"`
		Base        uint64 `json:"base"`
		Edits       []Edit `json:"edits"`
	}
	edits := batch.Edits
	if edits == nil {
		edits = []Edit{}
	}
	raw, err := json.Marshal(reference{stateDigest(head), batch.Base, edits})
	if err != nil {
		// The document is built from already-decoded values, so marshaling
		// cannot fail; keep the value deterministic if it ever does.
		return "invalid"
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func apply(g *graph.Graph, e Edit) (*RewriteReport, error) {
	if !e.onePayload() {
		return nil, graph.Fail(graph.Invalid, "exactly one edit payload is required")
	}
	switch {
	case e.Op == "put_vertex" && e.Vertex != nil:
		g.Vertices[e.Vertex.ID] = *e.Vertex
	case e.Op == "put_arc" && e.Arc != nil:
		g.Arcs[e.Arc.ID] = *e.Arc
	case e.Op == "set_schema" && e.Schema != nil:
		g.Schema = *e.Schema
	case e.Op == "rewrite_kind" && e.Rewrite != nil:
		report, err := e.Rewrite.apply(g)
		if err != nil {
			return nil, err
		}
		return &report, nil
	case e.Op == "drop_vertex" && e.ID != nil:
		if _, ok := g.Vertices[*e.ID]; !ok {
			return nil, graph.Fail(graph.Missing, "vertex %q does not exist", *e.ID)
		}
		delete(g.Vertices, *e.ID)
	case e.Op == "drop_arc" && e.ID != nil:
		if _, ok := g.Arcs[*e.ID]; !ok {
			return nil, graph.Fail(graph.Missing, "arc %q does not exist", *e.ID)
		}
		delete(g.Arcs, *e.ID)
	default:
		return nil, graph.Fail(graph.Invalid, "operation %q does not match its payload", e.Op)
	}
	return nil, nil
}

func (e Edit) onePayload() bool {
	count := 0
	for _, present := range []bool{e.ID != nil, e.Vertex != nil, e.Arc != nil, e.Schema != nil, e.Rewrite != nil} {
		if present {
			count++
		}
	}
	return count == 1
}
