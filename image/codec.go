package image

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"rillweave/graph"
)

const (
	Format     = 1
	MaxPayload = 12 << 20
	MaxImage   = (MaxPayload*4)/3 + 1024
)

type Envelope struct {
	Format  int    `json:"format"`
	Digest  string `json:"sha256"`
	Payload []byte `json:"payload"`
}

func Encode(g *graph.Graph) (Envelope, error) {
	payload, err := json.Marshal(g)
	if err != nil {
		return Envelope{}, err
	}
	if len(payload) > MaxPayload {
		return Envelope{}, graph.Fail(graph.Budget, "graph image exceeds %d payload bytes", MaxPayload)
	}
	return Envelope{Format, checksum(payload), payload}, nil
}

func Decode(e Envelope) (*graph.Graph, error) {
	switch {
	case e.Format != Format:
		return nil, graph.Fail(graph.Invalid, "unsupported graph image format %d", e.Format)
	case len(e.Payload) == 0 || len(e.Payload) > MaxPayload:
		return nil, graph.Fail(graph.Invalid, "image payload is empty or exceeds the size limit")
	case e.Digest != checksum(e.Payload):
		return nil, graph.Fail(graph.Invalid, "image digest mismatch")
	}
	var g graph.Graph
	if err := ReadJSON(bytes.NewReader(e.Payload), &g); err != nil {
		return nil, graph.Fail(graph.Invalid, "invalid graph payload: %v", err)
	}
	return &g, nil
}

func checksum(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func ReadJSON(r io.Reader, dst any) error {
	dec := json.NewDecoder(r)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	if err := dec.Decode(new(json.RawMessage)); err != io.EOF {
		return fmt.Errorf("expected one JSON object")
	}
	if len(raw) == 0 || raw[0] != '{' {
		return fmt.Errorf("expected a JSON object")
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	return strict.Decode(dst)
}
