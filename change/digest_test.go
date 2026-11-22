package change

import (
	"testing"

	"rillweave/graph"
)

// A preview digest must not transfer to a different graph that happens to
// share the base revision number.
func TestDigestBindsHeadContentNotRevision(t *testing.T) {
	head := base()
	batch := rewriteBatch()
	_, plan, err := PreparePlan(head, batch)
	if err != nil {
		t.Fatal(err)
	}

	other := head.Clone()
	vertex := other.Vertices["c"]
	vertex.Properties = map[string]graph.Scalar{"tag": sc(`"east"`)}
	other.Vertices["c"] = vertex
	if stateDigest(other) == stateDigest(head) {
		t.Fatal("content digest ignored a property change")
	}

	confirmed := batch
	confirmed.Confirm = &Confirm{Base: 2, Digest: plan.Digest}
	if _, _, err := PreparePlan(other, confirmed); err == nil {
		t.Fatal("confirmation accepted against different head content at the same revision")
	}
}

// The confirmation must also reject a digest computed for a different edit list.
func TestDigestBindsEdits(t *testing.T) {
	head := base()
	_, plan, err := PreparePlan(head, rewriteBatch())
	if err != nil {
		t.Fatal(err)
	}
	other := base()
	batch := Batch{Base: 2, Edits: []Edit{{Op: "put_vertex", Vertex: &graph.Vertex{ID: "a", Kind: "strut", Properties: map[string]graph.Scalar{"tag": sc(`"north"`)}}}}, Confirm: &Confirm{Base: 2, Digest: plan.Digest}}
	if _, _, err := PreparePlan(other, batch); err == nil {
		t.Fatal("confirmation accepted for a different edit list")
	}
}
