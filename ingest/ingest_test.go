package ingest

import (
	"testing"

	"rillweave/graph"
)

func TestClassifyPreservesScalarIdentity(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    graph.Kind
		problem string
		intVal  int64
		textVal string
		boolVal bool
	}{
		{"integer", `42`, graph.Int, "", 42, "", false},
		{"negative integer", `-7`, graph.Int, "", -7, "", false},
		{"quoted number is text", `"42"`, graph.Text, "", 0, "42", false},
		{"boolean", `true`, graph.Bool, "", 0, "", true},
		{"quoted boolean stays text", `"true"`, graph.Text, "", 0, "true", false},
		{"null rejected", `null`, "", ProbNull, 0, "", false},
		{"float rejected", `1.5`, "", ProbFloat, 0, "", false},
		{"integral float still rejected", `1.0`, "", ProbFloat, 0, "", false},
		{"exponent rejected", `1e3`, "", ProbFloat, 0, "", false},
		{"object rejected", `{"a":1}`, "", ProbNested, 0, "", false},
		{"array rejected", `[1]`, "", ProbNested, 0, "", false},
		{"empty token missing", "", "", ProbMissing, 0, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind, value, problem := Classify([]byte(tc.raw))
			if problem != tc.problem {
				t.Fatalf("problem = %q, want %q", problem, tc.problem)
			}
			if tc.problem != "" {
				return
			}
			if kind != tc.want {
				t.Fatalf("kind = %q, want %q", kind, tc.want)
			}
			switch kind {
			case graph.Int:
				if value.Int != tc.intVal {
					t.Fatalf("int = %d, want %d", value.Int, tc.intVal)
				}
			case graph.Text:
				if value.Text != tc.textVal {
					t.Fatalf("text = %q, want %q", value.Text, tc.textVal)
				}
			case graph.Bool:
				if value.Bool != tc.boolVal {
					t.Fatalf("bool = %v, want %v", value.Bool, tc.boolVal)
				}
			}
		})
	}
}

func TestClassifyNoImplicitWidening(t *testing.T) {
	// The same lexical digits must remain observably different by kind.
	_, asInt, problem := Classify([]byte(`5432`))
	if problem != "" || asInt.Kind != graph.Int || asInt.Int != 5432 {
		t.Fatalf("integer classification wrong: %+v %q", asInt, problem)
	}
	_, asText, problem := Classify([]byte(`"5432"`))
	if problem != "" || asText.Kind != graph.Text || asText.Text != "5432" {
		t.Fatalf("text classification wrong: %+v %q", asText, problem)
	}
}
