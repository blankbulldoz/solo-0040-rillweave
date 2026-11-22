package compress

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	"rillweave/graph"
)

// Encode serializes a validated compressed document into a digest-protected
// envelope. The digest establishes byte integrity of the portable summary.
func Encode(d *Document) (Envelope, error) {
	if err := Validate(d); err != nil {
		return Envelope{}, err
	}
	payload, err := json.Marshal(d)
	if err != nil {
		return Envelope{}, err
	}
	if len(payload) > MaxPayload {
		return Envelope{}, graph.Fail(graph.Budget, "compressed image exceeds %d payload bytes", MaxPayload)
	}
	return Envelope{Format, checksum(payload), payload}, nil
}

// Decode verifies envelope integrity and returns the compressed document.
func Decode(e Envelope) (*Document, error) {
	switch {
	case e.Format != Format:
		return nil, graph.Fail(graph.Invalid, "unsupported compressed image format %d", e.Format)
	case len(e.Payload) == 0 || len(e.Payload) > MaxPayload:
		return nil, graph.Fail(graph.Invalid, "compressed payload is empty or exceeds the size limit")
	case e.Digest != checksum(e.Payload):
		return nil, graph.Fail(graph.Invalid, "compressed image digest mismatch")
	}
	var d Document
	if err := ReadJSON(bytes.NewReader(e.Payload), &d); err != nil {
		return nil, graph.Fail(graph.Invalid, "invalid compressed payload: %v", err)
	}
	if err := Validate(&d); err != nil {
		return nil, err
	}
	return &d, nil
}

func checksum(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ReadJSON decodes exactly one strict JSON object, reusing the full image rule.
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
