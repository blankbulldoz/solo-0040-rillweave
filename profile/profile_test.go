package profile

import (
	"encoding/json"
	"testing"

	"rillweave/graph"
	"rillweave/ingest"
)

func file(t *testing.T, path string, export ingest.Export) ingest.InputFile {
	t.Helper()
	raw, err := json.Marshal(export)
	if err != nil {
		t.Fatal(err)
	}
	var decoded ingest.Export
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	return ingest.InputFile{Path: path, SHA256: "test-" + path, Export: &decoded}
}

func TestMissingIsNotAType(t *testing.T) {
	files := []ingest.InputFile{file(t, "one.json", ingest.Export{
		Vertices: []ingest.RawVertex{
			{ID: "n1", Kind: "node", Properties: map[string]json.RawMessage{
				"flag": json.RawMessage(`true`),
			}},
			{ID: "n2", Kind: "node", Properties: map[string]json.RawMessage{
				"flag": json.RawMessage(`false`),
			}},
			{ID: "n3", Kind: "node", Properties: map[string]json.RawMessage{
				"other": json.RawMessage(`"x"`),
			}},
		},
		Arcs: []ingest.RawArc{
			{ID: "a1", Kind: "link", From: "n1", To: "n2", Cost: json.RawMessage(`1`)},
		},
	})}
	p := Build(files)
	node := p.VertexKinds["node"]
	flag := node.Properties["flag"]
	if flag.Present != 2 || flag.Missing != 1 {
		t.Fatalf("flag present/missing = %d/%d, want 2/1", flag.Present, flag.Missing)
	}
	if len(flag.Types) != 1 || flag.Types[0].Kind != graph.Bool {
		t.Fatalf("flag must be uniformly bool, got %v", flag.Types)
	}
	if p.Counts.Blockers != 0 || p.Counts.Conflicts != 0 {
		t.Fatalf("clean input should not produce findings, got %+v", p.Findings)
	}
}

func TestNullIsBlockerWithRawEvidence(t *testing.T) {
	files := []ingest.InputFile{file(t, "one.json", ingest.Export{
		Vertices: []ingest.RawVertex{
			{ID: "n1", Kind: "node", Properties: map[string]json.RawMessage{
				"maybe": json.RawMessage(`null`),
			}},
		},
		Arcs: []ingest.RawArc{
			{ID: "a1", Kind: "link", From: "n1", To: "n1", Cost: json.RawMessage(`1`)},
		},
	})}
	p := Build(files)
	var found bool
	for _, f := range p.Findings {
		if f.Code == UnsupportedScalar && f.Severity == SeverityBlocker {
			found = true
			if len(f.Evidence) == 0 || f.Evidence[0].Raw != "null" {
				t.Fatalf("null evidence must preserve the raw token, got %+v", f.Evidence)
			}
		}
	}
	if !found {
		t.Fatalf("expected an unsupported_scalar blocker for null, got %+v", p.Findings)
	}
}

func TestArcEndpointPairConflictCarriesPlans(t *testing.T) {
	files := []ingest.InputFile{file(t, "one.json", ingest.Export{
		Vertices: []ingest.RawVertex{
			{ID: "s1", Kind: "service"},
			{ID: "d1", Kind: "database"},
		},
		Arcs: []ingest.RawArc{
			{ID: "a1", Kind: "depends_on", From: "s1", To: "d1", Cost: json.RawMessage(`1`)},
			{ID: "a2", Kind: "depends_on", From: "d1", To: "s1", Cost: json.RawMessage(`2`)},
		},
	})}
	p := Build(files)
	var conflict *Finding
	for i := range p.Findings {
		if p.Findings[i].Code == ArcEndpointConflict {
			conflict = &p.Findings[i]
		}
	}
	if conflict == nil {
		t.Fatalf("expected an endpoint pair conflict")
	}
	if len(conflict.Plans) < 2 {
		t.Fatalf("expected multiple split plans, got %d", len(conflict.Plans))
	}
	ap := p.ArcKinds["depends_on"]
	if len(ap.Pairs) != 2 {
		t.Fatalf("expected two endpoint pairs, got %+v", ap.Pairs)
	}
}

func TestDigestStableAcrossBuilds(t *testing.T) {
	mk := func() []ingest.InputFile {
		return []ingest.InputFile{file(t, "one.json", ingest.Export{
			Vertices: []ingest.RawVertex{{ID: "n1", Kind: "node"}},
			Arcs:     []ingest.RawArc{{ID: "a1", Kind: "link", From: "n1", To: "n1", Cost: json.RawMessage(`1`)}},
		})}
	}
	first := Build(mk()).Digest
	second := Build(mk()).Digest
	if first == "" || first != second {
		t.Fatalf("profile digest must be stable for identical inputs: %q vs %q", first, second)
	}
}
