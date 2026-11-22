package catalog

import (
	"testing"

	"rillweave/graph"
	"rillweave/index"
)

func planSchema() graph.Schema {
	return graph.Schema{
		Vertices: map[string]graph.VertexRule{
			// The same property name carries different scalar kinds per kind.
			"hinge": {Properties: map[string]graph.PropertyRule{
				"tag":  {Kind: graph.Text},
				"tier": {Kind: graph.Int},
			}},
			"keel": {Properties: map[string]graph.PropertyRule{
				"tag":  {Kind: graph.Text},
				"tier": {Kind: graph.Bool},
			}},
		},
		Arcs: map[string]graph.ArcRule{"join": {From: "hinge", To: "keel"}},
	}
}

func TestLookupPlanAccepts(t *testing.T) {
	s := planSchema()
	terms := []index.Term{
		{Match: index.Match{Kind: "hinge", Property: "tier", Value: graph.Scalar{Kind: graph.Int, Int: 1}}},
		{Match: index.Match{Kind: "keel", Property: "tier", Value: graph.Scalar{Kind: graph.Bool, Bool: true}}, Op: index.Union},
		{Match: index.Match{Kind: "hinge", Property: "tag", Value: graph.Scalar{Kind: graph.Text, Text: "x"}}, Op: index.Subtract},
	}
	if err := LookupPlan(s, terms); err != nil {
		t.Fatalf("valid cross-kind plan rejected: %v", err)
	}
}

func TestLookupPlanRejectsIllegalRightEvenWhenLeftEmpty(t *testing.T) {
	s := planSchema()
	// First two terms provably subtract the whole north posting away; the
	// third term is an illegal int on a bool field and must still be checked.
	terms := []index.Term{
		{Match: index.Match{Kind: "hinge", Property: "tag", Value: graph.Scalar{Kind: graph.Text, Text: "north"}}},
		{Match: index.Match{Kind: "hinge", Property: "tag", Value: graph.Scalar{Kind: graph.Text, Text: "north"}}, Op: index.Subtract},
		{Match: index.Match{Kind: "keel", Property: "tier", Value: graph.Scalar{Kind: graph.Int, Int: 7}}, Op: index.Subtract},
	}
	err := LookupPlan(s, terms)
	if err == nil {
		t.Fatal("illegal right-hand condition accepted after empty left side")
	}
	if problem, ok := err.(*graph.Problem); !ok || problem.Code != graph.Invalid {
		t.Fatalf("want invalid problem, got %v", err)
	}
}

func TestLookupPlanStructure(t *testing.T) {
	s := planSchema()
	text := func(v string) graph.Scalar { return graph.Scalar{Kind: graph.Text, Text: v} }
	mk := func(op index.Operation) index.Term {
		return index.Term{
			Match: index.Match{Kind: "hinge", Property: "tag", Value: text("x")}, Op: op,
		}
	}
	if err := LookupPlan(s, nil); err == nil {
		t.Fatal("empty plan must be rejected")
	}
	missingOp := mk(index.Union)
	missingOp.Op = ""
	if err := LookupPlan(s, []index.Term{mk(""), missingOp}); err == nil {
		t.Fatal("missing op on second term must be rejected")
	}
	if err := LookupPlan(s, []index.Term{mk(""), mk("xor")}); err == nil {
		t.Fatal("unknown op must be rejected")
	}
	over := make([]index.Term, index.MaxLookupConditions+1)
	for i := range over {
		over[i] = mk(index.Union)
	}
	over[0].Op = ""
	if err := LookupPlan(s, over); err == nil {
		t.Fatalf("more than %d conditions must be rejected", index.MaxLookupConditions)
	}
}

func TestLookupPlanUnknownKindAndProperty(t *testing.T) {
	s := planSchema()
	terms := []index.Term{{Match: index.Match{
		Kind: "strut", Property: "tag", Value: graph.Scalar{Kind: graph.Text, Text: "x"},
	}}}
	if err := LookupPlan(s, terms); err == nil {
		t.Fatal("unknown vertex kind must be rejected")
	}
	terms[0].Kind = "hinge"
	terms[0].Property = "missing"
	if err := LookupPlan(s, terms); err == nil {
		t.Fatal("unknown property must be rejected")
	}
	terms[0].Property = "tier"
	terms[0].Value = graph.Scalar{Kind: graph.Bool, Bool: true}
	if err := LookupPlan(s, terms); err == nil {
		t.Fatal("wrong scalar kind must be rejected")
	}
}
