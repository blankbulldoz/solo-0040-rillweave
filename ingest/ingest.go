// Package ingest decodes externally exported vertex/arc JSON without
// changing scalar representations. Property values stay raw JSON tokens
// until a caller classifies them, so text "1", integer 1, boolean true,
// 1.5 and null remain observably distinct.
package ingest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"rillweave/graph"
)

// RawVertex is one exported vertex record. Properties keep their raw JSON
// form so classification never depends on Go's float64 default.
type RawVertex struct {
	ID         string                     `json:"id"`
	Kind       string                     `json:"kind"`
	Properties map[string]json.RawMessage `json:"properties,omitempty"`
}

// RawArc is one exported arc record. Cost stays raw because an external
// export may omit it or encode a non-integer value.
type RawArc struct {
	ID         string                     `json:"id"`
	Kind       string                     `json:"kind"`
	From       string                     `json:"from"`
	To         string                     `json:"to"`
	Cost       json.RawMessage            `json:"cost,omitempty"`
	Properties map[string]json.RawMessage `json:"properties,omitempty"`
}

// Export is the supported external contract: one JSON object holding
// vertex and arc arrays. Either array may be empty.
type Export struct {
	Vertices []RawVertex `json:"vertices,omitempty"`
	Arcs     []RawArc    `json:"arcs,omitempty"`
}

// Ref locates one record inside the scanned input set.
type Ref struct {
	File    string `json:"file"`
	Ordinal int    `json:"ordinal"`
	ID      string `json:"id"`
}

// InputFile is one decoded export plus the checksum of its exact bytes.
type InputFile struct {
	Path   string
	SHA256 string
	Export *Export
}

const maxEvidenceRaw = 128

// Evidence is a traceable record reference carrying the original scalar
// token (truncated for artifact size), which is what lets a reviewer see
// that "123" arrived as text rather than being converted from an integer.
type Evidence struct {
	Ref
	Raw string `json:"raw,omitempty"`
}

func EvidenceFor(ref Ref, raw json.RawMessage) Evidence {
	token := strings.TrimSpace(string(raw))
	if len(token) > maxEvidenceRaw {
		token = token[:maxEvidenceRaw] + "…"
	}
	return Evidence{Ref: ref, Raw: token}
}

// Scalar problem codes returned by Classify. Empty means the token is a
// valid Rillweave scalar.
const (
	ProbMissing      = "missing"
	ProbNull         = "null_not_supported"
	ProbNested       = "nested_not_supported"
	ProbFloat        = "float_not_supported"
	ProbNumberRange  = "number_out_of_int64"
	ProbTextTooLarge = "text_exceeds_1024_bytes"
	ProbInvalidToken = "invalid_json_token"
)

// Classify inspects a raw property token and returns its observed scalar
// kind plus the typed value. It never coerces: a quoted number is text,
// a decimal or exponent token is rejected, and null is rejected.
func Classify(raw json.RawMessage) (graph.Kind, graph.Scalar, string) {
	token := bytes.TrimSpace(raw)
	if len(token) == 0 {
		return "", graph.Scalar{}, ProbMissing
	}
	switch token[0] {
	case '"':
		var text string
		if err := json.Unmarshal(token, &text); err != nil {
			return "", graph.Scalar{}, ProbInvalidToken
		}
		value := graph.Scalar{Kind: graph.Text, Text: text}
		if !value.Valid() {
			return graph.Text, value, ProbTextTooLarge
		}
		return graph.Text, value, ""
	case 't', 'f':
		if bytes.Equal(token, []byte("true")) || bytes.Equal(token, []byte("false")) {
			return graph.Bool, graph.Scalar{Kind: graph.Bool, Bool: token[0] == 't'}, ""
		}
		return "", graph.Scalar{}, ProbInvalidToken
	case 'n':
		if bytes.Equal(token, []byte("null")) {
			return "", graph.Scalar{}, ProbNull
		}
		return "", graph.Scalar{}, ProbInvalidToken
	case '{', '[':
		return "", graph.Scalar{}, ProbNested
	default:
		literal := string(token)
		// A decimal point or exponent marks a float even when the value
		// happens to be integral (for example 1.0); no silent narrowing.
		if strings.ContainsAny(literal, ".eE") {
			return "", graph.Scalar{}, ProbFloat
		}
		value, err := strconv.ParseInt(literal, 10, 64)
		if err != nil {
			return "", graph.Scalar{}, ProbNumberRange
		}
		return graph.Int, graph.Scalar{Kind: graph.Int, Int: value}, ""
	}
}

// Expand resolves command line inputs: directories expand to their
// non-hidden .json files in sorted order; explicit files pass through.
func Expand(args []string) ([]string, error) {
	var paths []string
	for _, arg := range args {
		info, err := os.Stat(arg)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			paths = append(paths, arg)
			continue
		}
		entries, err := os.ReadDir(arg)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.Type().IsRegular() && !strings.HasPrefix(name, ".") && strings.HasSuffix(name, ".json") {
				paths = append(paths, filepath.Join(arg, name))
			}
		}
	}
	sort.Strings(paths)
	uniq := paths[:0]
	for _, p := range paths {
		if len(uniq) == 0 || uniq[len(uniq)-1] != p {
			uniq = append(uniq, p)
		}
	}
	return uniq, nil
}

// ReadFile decodes one export file strictly: exactly one JSON object, no
// unknown record fields, and checksummed bytes for later staleness checks.
func ReadFile(path string) (InputFile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return InputFile{}, err
	}
	decoded, err := decodeStrict(raw)
	if err != nil {
		return InputFile{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := checkShapes(path, decoded); err != nil {
		return InputFile{}, err
	}
	sum := sha256.Sum256(raw)
	return InputFile{Path: path, SHA256: hex.EncodeToString(sum[:]), Export: decoded}, nil
}

func decodeStrict(raw []byte) (*Export, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	var head json.RawMessage
	if err := dec.Decode(&head); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return nil, fmt.Errorf("expected one JSON object")
	}
	if len(head) == 0 || head[0] != '{' {
		return nil, fmt.Errorf("expected an object with vertices and arcs")
	}
	strict := json.NewDecoder(bytes.NewReader(head))
	strict.DisallowUnknownFields()
	var export Export
	if err := strict.Decode(&export); err != nil {
		return nil, fmt.Errorf("invalid export record: %w", err)
	}
	return &export, nil
}

func checkShapes(path string, export *Export) error {
	for i, v := range export.Vertices {
		if v.ID == "" || v.Kind == "" {
			return fmt.Errorf("%s: vertices[%d] requires id and kind", path, i)
		}
	}
	for i, a := range export.Arcs {
		if a.ID == "" || a.Kind == "" || a.From == "" || a.To == "" {
			return fmt.Errorf("%s: arcs[%d] requires id, kind, from and to", path, i)
		}
	}
	return nil
}
