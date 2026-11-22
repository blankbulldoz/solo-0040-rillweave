// Command rillweave-collate checks whether two imported Rillweave graph images
// are logically equivalent despite renamed vertices and arcs. Each input is an
// image envelope (format/sha256/payload, as returned by GET .../image) or a
// raw graph JSON document. Anchors may fix part of the vertex correspondence.
//
// Exit status: 0 equivalent, 3 inequivalent, 4 undetermined, 2 usage/input
// error. The digest is never treated as a premise.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"rillweave/collate"
	"rillweave/graph"
	"rillweave/image"
)

func main() {
	var (
		anchorSpec = flag.String("anchors", "", "comma-separated left=right vertex anchor pairs")
		budget     = flag.Int("budget", collate.DefaultBudget, "maximum complete assignment attempts before returning undetermined")
	)
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [--anchors left=right,...] [--budget n] left-image.json right-image.json\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}

	anchors, err := parseAnchors(*anchorSpec)
	if err != nil {
		fail(2, err)
	}
	left, err := loadGraph(flag.Arg(0))
	if err != nil {
		fail(2, fmt.Errorf("left image: %w", err))
	}
	right, err := loadGraph(flag.Arg(1))
	if err != nil {
		fail(2, fmt.Errorf("right image: %w", err))
	}

	result := collate.Equivalent(left, right, anchors, collate.Budget{Assignments: *budget})
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fail(2, err)
	}
	fmt.Println(string(encoded))
	switch result.Status {
	case collate.StatusEquivalent:
		os.Exit(0)
	case collate.StatusUndetermined:
		os.Exit(4)
	default:
		os.Exit(3)
	}
}

func parseAnchors(spec string) ([]collate.Anchor, error) {
	if strings.TrimSpace(spec) == "" {
		return nil, nil
	}
	anchors := []collate.Anchor{}
	for _, pair := range strings.Split(spec, ",") {
		pair = strings.TrimSpace(pair)
		left, right, found := strings.Cut(pair, "=")
		if !found || strings.TrimSpace(left) == "" || strings.TrimSpace(right) == "" {
			return nil, fmt.Errorf("invalid anchor %q, expected left=right", pair)
		}
		anchors = append(anchors, collate.Anchor{Left: strings.TrimSpace(left), Right: strings.TrimSpace(right)})
	}
	return anchors, nil
}

// loadGraph reads either a full image envelope or a raw graph document. A raw
// graph document is wrapped as a syntactically valid payload without checking
// any digest, since the digest is explicitly not a premise of equivalence.
func loadGraph(path string) (*graph.Graph, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		Format  int             `json:"format"`
		Digest  string          `json:"sha256"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if len(envelope.Payload) != 0 {
		// An exported image stores the graph payload as a base64 JSON string.
		var encoded string
		if err := json.Unmarshal(envelope.Payload, &encoded); err == nil {
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				return nil, fmt.Errorf("image payload base64: %w", err)
			}
			return decodeGraph(decoded)
		}
		return decodeGraph(envelope.Payload)
	}
	return decodeGraph(raw)
}

func decodeGraph(raw []byte) (*graph.Graph, error) {
	var g graph.Graph
	if err := image.ReadJSON(bytes.NewReader(raw), &g); err != nil {
		return nil, err
	}
	if g.Vertices == nil || g.Arcs == nil {
		return nil, fmt.Errorf("document is not a graph")
	}
	return &g, nil
}

func fail(code int, err error) {
	fmt.Fprintln(os.Stderr, "rillweave-collate:", err)
	os.Exit(code)
}
