package change

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"rillweave/graph"
)

func sc(t string) graph.Scalar {
	var s graph.Scalar
	if err := json.Unmarshal([]byte(t), &s); err != nil {
		panic(err)
	}
	return s
}

func tagRule() map[string]graph.PropertyRule {
	return map[string]graph.PropertyRule{"tag": {Kind: graph.Text}}
}

func base() *graph.Graph {
	g := graph.New("g", graph.Schema{
		Vertices: map[string]graph.VertexRule{
			"strut": {Properties: tagRule()},
			"pier":  {Properties: tagRule()},
			"deck":  {Properties: tagRule()},
			"cap":   {Properties: tagRule()},
		},
		Arcs: map[string]graph.ArcRule{
			"join": {From: "strut", To: "strut"},
			"up":   {From: "pier", To: "strut"},
			"over": {From: "pier", To: "deck"},
			"on":   {From: "deck", To: "strut"},
			"rest": {From: "cap", To: "pier"},
		},
	})
	g.Revision = 2
	g.Vertices["a"] = graph.Vertex{ID: "a", Kind: "strut", Properties: map[string]graph.Scalar{"tag": sc(`"north"`)}}
	g.Vertices["b"] = graph.Vertex{ID: "b", Kind: "strut", Properties: map[string]graph.Scalar{"tag": sc(`"south"`)}}
	g.Vertices["c"] = graph.Vertex{ID: "c", Kind: "strut"}
	g.Arcs["e1"] = graph.Arc{ID: "e1", Kind: "join", From: "a", To: "b", Cost: 1}
	g.Arcs["e2"] = graph.Arc{ID: "e2", Kind: "join", From: "c", To: "a", Cost: 2}
	return g
}

func rewriteBatch() Batch {
	return Batch{Base: 2, Edits: []Edit{{Op: "rewrite_kind", Rewrite: &Rewrite{
		Vertex: "strut",
		Classes: []VertexClass{
			{Kind: "pier", When: []Match{{Property: "tag", Equals: sc(`"north"`)}}},
			{Kind: "deck", When: []Match{{Property: "tag", Equals: sc(`"south"`)}}},
		},
		Default: "cap",
		Arcs: []ArcRule{
			{Kind: "join", To: []ArcTarget{{Kind: "up"}, {Kind: "over"}, {Kind: "on"}, {Kind: "rest"}}},
		},
	}}}}
}

func TestRewriteSplitsPerInstance(t *testing.T) {
	g := base()
	next, plan, err := PreparePlan(g, rewriteBatch())
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if next.Vertices["a"].Kind != "pier" || next.Vertices["b"].Kind != "deck" || next.Vertices["c"].Kind != "cap" {
		t.Fatalf("unexpected kinds: %+v", next.Vertices)
	}
	if next.Vertices["a"].Properties["tag"] != sc(`"north"`) {
		t.Fatal("properties changed")
	}
	if next.Arcs["e1"].Kind != "over" || next.Arcs["e2"].Kind != "rest" {
		t.Fatalf("arcs not uniquely converted: %+v", next.Arcs)
	}
	if plan.Digest == "" || plan.NextRevision != 3 {
		t.Fatalf("bad plan: %+v", plan)
	}
	if g.Vertices["a"].Kind != "strut" {
		t.Fatal("input graph mutated")
	}
}

func TestRewriteAmbiguousVertexReportsEndpointMaterial(t *testing.T) {
	g := base()
	batch := rewriteBatch()
	batch.Edits[0].Rewrite.Classes = append(batch.Edits[0].Rewrite.Classes,
		VertexClass{Kind: "cap", When: []Match{{Property: "tag", Equals: sc(`"north"`)}}})
	_, _, err := PreparePlan(g, batch)
	var problem *graph.Problem
	if !errors.As(err, &problem) || problem.Code != graph.Conflict {
		t.Fatalf("want conflict, got %v", err)
	}
	raw, _ := json.Marshal(problem)
	text := string(raw)
	for _, want := range []string{`"reason":"ambiguous"`, `"matched_kinds":["cap","pier"]`, `"id":"a"`, `"arc":"e1"`, `"from_endpoint"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("witness missing %s: %s", want, text)
		}
	}
}

func TestRewriteAmbiguousArc(t *testing.T) {
	g := base()
	batch := rewriteBatch()
	batch.Edits[0].Rewrite.Arcs[0].To = []ArcTarget{{Kind: "up"}, {Kind: "over", When: []Match{{Property: "weight", Equals: sc(`1`)}}}}
	batch.Edits[0].Rewrite.Arcs[0].To = []ArcTarget{{Kind: "up"}}
	// endpoints pier->deck matches only over, not up; should be no compatible.
	_, _, err := PreparePlan(g, batch)
	var problem *graph.Problem
	errors.As(err, &problem)
	if problem == nil || problem.Code != graph.Conflict {
		t.Fatalf("want conflict, got %v", err)
	}
	raw, _ := json.Marshal(problem)
	if !strings.Contains(string(raw), `"reason":"no_compatible_kind"`) || !strings.Contains(string(raw), `"to_endpoint"`) {
		t.Fatalf("bad witness: %s", raw)
	}
}

func TestConfirmDigestBindsPreview(t *testing.T) {
	g := base()
	batch := rewriteBatch()
	_, plan, err := PreparePlan(g, batch)
	if err != nil {
		t.Fatal(err)
	}
	confirmed := batch
	confirmed.Confirm = &Confirm{Base: 2, Digest: plan.Digest}
	if _, _, err := PreparePlan(g, confirmed); err != nil {
		t.Fatalf("confirmed commit rejected: %v", err)
	}

	changed := batch
	changed.Edits = append([]Edit{}, batch.Edits...)
	changed.Confirm = &Confirm{Base: 2, Digest: plan.Digest}
	changed.Edits[0].Rewrite = &Rewrite{Vertex: "strut", Classes: []VertexClass{{Kind: "pier", When: []Match{{Property: "tag", Equals: sc(`"north"`)}}}}, Default: "cap"}
	_, _, err = PreparePlan(g, changed)
	if err == nil || !strings.Contains(err.Error(), "preview") {
		t.Fatalf("changed edits accepted with stale digest: %v", err)
	}

	// Head advanced: same preview base is no longer current.
	advanced := base()
	advanced.Revision = 3
	if _, _, err := PreparePlan(advanced, confirmed); err == nil {
		t.Fatal("stale base accepted")
	}
}

func TestRewriteLeavesUnlistedArcsAlone(t *testing.T) {
	g := base()
	r := rewriteBatch()
	r.Edits[0].Rewrite.Arcs = nil
	_, _, err := PreparePlan(g, r)
	var problem *graph.Problem
	errors.As(err, &problem)
	if problem == nil || problem.Code != graph.Conflict {
		t.Fatalf("unlisted arc invalidated silently: %v", err)
	}
	raw, _ := json.Marshal(problem)
	if !strings.Contains(string(raw), `"reason":"unlisted_endpoint_mismatch"`) {
		t.Fatalf("bad witness: %s", raw)
	}
}
