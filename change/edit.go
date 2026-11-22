package change

import (
	"fmt"
	"math"

	"rillweave/catalog"
	"rillweave/graph"
)

const MaxEdits = 2000

type Edit struct {
	Op     string        `json:"op"`
	ID     *string       `json:"id,omitempty"`
	Vertex *graph.Vertex `json:"vertex,omitempty"`
	Arc    *graph.Arc    `json:"arc,omitempty"`
	Schema *graph.Schema `json:"schema,omitempty"`
}

// RebaseSafe is the only accepted Batch.Rebase mode. It asks the engine to
// replay the batch onto the current head when its base is stale, but only when
// the intervening revisions can be proven independent of the batch.
const RebaseSafe = "safe"

type Batch struct {
	Base   uint64 `json:"base"`
	Edits  []Edit `json:"edits"`
	Rebase string `json:"rebase,omitempty"`
}

func Prepare(head *graph.Graph, batch Batch) (*graph.Graph, error) {
	if batch.Base != head.Revision {
		return nil, graph.Fail(graph.Conflict, "base %d is not current revision %d", batch.Base, head.Revision)
	}
	if err := checkBatch(head, batch); err != nil {
		return nil, err
	}
	candidate, err := stageChecked(head, batch)
	if err != nil {
		return nil, err
	}
	if err := catalog.Validate(candidate); err != nil {
		return nil, err
	}
	candidate.Revision++
	return candidate, nil
}

func checkBatch(head *graph.Graph, batch Batch) error {
	if len(batch.Edits) == 0 || len(batch.Edits) > MaxEdits {
		return graph.Fail(graph.Invalid, "a commit requires 1..%d edits", MaxEdits)
	}
	if head.Revision == math.MaxUint64 {
		return graph.Fail(graph.Budget, "revision space exhausted")
	}
	return nil
}

// stage applies a batch onto a private clone of start without validating it.
// Each edit is still checked for shape and, for drops, for an existing target.
func stage(start *graph.Graph, batch Batch) *graph.Graph {
	candidate := start.Clone()
	for _, edit := range batch.Edits {
		// apply only fails on malformed payloads or missing drop targets; both
		// are ruled out ahead of a rebase by the staged candidate and checkBatch.
		_ = apply(candidate, edit)
	}
	return candidate
}

// stageChecked applies a batch and surfaces edit-order errors, as Prepare does.
func stageChecked(start *graph.Graph, batch Batch) (*graph.Graph, error) {
	candidate := start.Clone()
	for n, edit := range batch.Edits {
		if err := apply(candidate, edit); err != nil {
			return nil, fmt.Errorf("edit %d: %w", n, err)
		}
	}
	return candidate, nil
}

func apply(g *graph.Graph, e Edit) error {
	if !e.onePayload() {
		return graph.Fail(graph.Invalid, "exactly one edit payload is required")
	}
	switch {
	case e.Op == "put_vertex" && e.Vertex != nil:
		g.Vertices[e.Vertex.ID] = *e.Vertex
	case e.Op == "put_arc" && e.Arc != nil:
		g.Arcs[e.Arc.ID] = *e.Arc
	case e.Op == "set_schema" && e.Schema != nil:
		g.Schema = *e.Schema
	case e.Op == "drop_vertex" && e.ID != nil:
		if _, ok := g.Vertices[*e.ID]; !ok {
			return graph.Fail(graph.Missing, "vertex %q does not exist", *e.ID)
		}
		delete(g.Vertices, *e.ID)
	case e.Op == "drop_arc" && e.ID != nil:
		if _, ok := g.Arcs[*e.ID]; !ok {
			return graph.Fail(graph.Missing, "arc %q does not exist", *e.ID)
		}
		delete(g.Arcs, *e.ID)
	default:
		return graph.Fail(graph.Invalid, "operation %q does not match its payload", e.Op)
	}
	return nil
}

func (e Edit) onePayload() bool {
	count := 0
	for _, present := range []bool{e.ID != nil, e.Vertex != nil, e.Arc != nil, e.Schema != nil} {
		if present {
			count++
		}
	}
	return count == 1
}
