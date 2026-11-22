package assemble

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"rillweave/graph"
	"rillweave/ingest"
	"rillweave/profile"
)

// exportFile builds an in-memory InputFile the same way the CLI does,
// including a checksum over the exact bytes.
func exportFile(t *testing.T, path string, export ingest.Export) ingest.InputFile {
	t.Helper()
	raw, err := json.Marshal(export)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := func() (*ingest.Export, error) {
		// Round-trip through strict decoding to honor the production path.
		var e ingest.Export
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, err
		}
		return &e, nil
	}()
	if err != nil {
		t.Fatal(err)
	}
	return ingest.InputFile{Path: path, SHA256: checksum(raw), Export: decoded}
}

func checksum(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func conflictingInputs(t *testing.T) []ingest.InputFile {
	first := ingest.Export{
		Vertices: []ingest.RawVertex{
			{ID: "svc-1", Kind: "service", Properties: map[string]json.RawMessage{
				"replicas": json.RawMessage(`3`),
				"name":     json.RawMessage(`"billing"`),
			}},
			{ID: "db-1", Kind: "database", Properties: map[string]json.RawMessage{
				"port": json.RawMessage(`"5432"`),
			}},
		},
		Arcs: []ingest.RawArc{
			{ID: "edge-1", Kind: "depends_on", From: "svc-1", To: "db-1", Cost: json.RawMessage(`4`),
				Properties: map[string]json.RawMessage{"weight": json.RawMessage(`1`)}},
			{ID: "edge-2", Kind: "depends_on", From: "db-1", To: "svc-1", Cost: json.RawMessage(`2`),
				Properties: map[string]json.RawMessage{"weight": json.RawMessage(`"2"`)}},
		},
	}
	second := ingest.Export{
		Vertices: []ingest.RawVertex{
			{ID: "svc-2", Kind: "service", Properties: map[string]json.RawMessage{
				"replicas": json.RawMessage(`"5"`),
			}},
			{ID: "db-2", Kind: "database", Properties: map[string]json.RawMessage{
				"port": json.RawMessage(`5432`),
			}},
		},
	}
	return []ingest.InputFile{
		exportFile(t, "exports/part1.json", first),
		exportFile(t, "exports/part2.json", second),
	}
}

func TestProfileFindsTypeAndEndpointConflicts(t *testing.T) {
	files := conflictingInputs(t)
	p := profile.Build(files)

	var sawType, sawEndpoint, sawArcType bool
	for _, f := range p.Findings {
		switch f.Code {
		case profile.VertexTypeConflict:
			sawType = true
			if len(f.Evidence) < 2 {
				t.Fatalf("type conflict %s lacks per-kind evidence", f.ID)
			}
			if len(f.Plans) < 2 {
				t.Fatalf("type conflict %s lacks split plans", f.ID)
			}
		case profile.ArcEndpointConflict:
			sawEndpoint = true
		case profile.ArcTypeConflict:
			sawArcType = true
		}
	}
	if !sawType || !sawEndpoint || !sawArcType {
		t.Fatalf("expected all three conflict classes, got type=%v endpoint=%v arcType=%v",
			sawType, sawEndpoint, sawArcType)
	}
	if p.Counts.Conflicts < 3 {
		t.Fatalf("expected at least 3 conflicts, got %d", p.Counts.Conflicts)
	}
}

func TestStarterDecisionsBuildValidatedGraph(t *testing.T) {
	files := conflictingInputs(t)
	p := profile.Build(files)
	d := StarterDecisions(p, "confirmed-graph")

	if err := ValidateDecisions(p, d); err != nil {
		t.Fatal(err)
	}
	g, report, err := Build(p, d, files)
	if err != nil {
		t.Fatalf("build failed: %v", err)
	}
	if report.CatalogCheck != "passed" {
		t.Fatalf("catalog check = %q", report.CatalogCheck)
	}
	if g.Name != "confirmed-graph" {
		t.Fatalf("graph name = %q", g.Name)
	}
	// The port conflict must have produced two typed properties.
	dbRule := g.Schema.Vertices["database"]
	if _, ok := dbRule.Properties["port_int"]; !ok {
		t.Fatalf("expected port_int rule, got %v", dbRule.Properties)
	}
	if _, ok := dbRule.Properties["port_text"]; !ok {
		t.Fatalf("expected port_text rule, got %v", dbRule.Properties)
	}
	if _, leftover := dbRule.Properties["port"]; leftover {
		t.Fatalf("unsplit port property must not remain in the schema")
	}
	// Arc kind must be split by endpoint pair.
	if len(g.Schema.Arcs) < 2 {
		t.Fatalf("expected split arc kinds, got %v", g.Schema.Arcs)
	}
}

func TestStaleDecisionsRejected(t *testing.T) {
	files := conflictingInputs(t)
	p := profile.Build(files)
	d := StarterDecisions(p, "confirmed-graph")
	d.ProfileDigest = "deadbeef"
	if _, _, err := Build(p, d, files); err == nil ||
		!strings.Contains(err.Error(), "does not match profile") {
		t.Fatalf("expected digest mismatch error, got %v", err)
	}
}

func TestMutatedInputRejected(t *testing.T) {
	files := conflictingInputs(t)
	p := profile.Build(files)
	files[0].SHA256 = "changed"
	if err := VerifyInputs(p, files); err == nil ||
		!strings.Contains(err.Error(), "changed since inference") {
		t.Fatalf("expected checksum guard error, got %v", err)
	}
}

func TestExplicitCastRequiresOwnerNote(t *testing.T) {
	files := conflictingInputs(t)
	p := profile.Build(files)
	d := StarterDecisions(p, "cast-graph")
	// Remove the port split and attempt an undocumented cast.
	filtered := d.Actions[:0]
	for _, act := range d.Actions {
		if act.Op == "split_vertex_property" && act.Property == "port" {
			continue
		}
		filtered = append(filtered, act)
	}
	d.Actions = append(filtered, Action{
		Op: "explicit_cast", EntityKind: "database", Property: "port", NewName: string(graph.Int),
	})
	if _, _, err := Build(p, d, files); err == nil ||
		!strings.Contains(err.Error(), "owner_note") {
		t.Fatalf("expected owner_note requirement, got %v", err)
	}
}
