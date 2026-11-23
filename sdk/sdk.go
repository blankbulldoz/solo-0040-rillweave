// Package sdk migrates raw /rill/v1 commit requests to the /rill/v2 typed
// operation envelope. The v2 form carries every 64-bit integer as a decimal
// string, so a request survives a browser's JSON parse and re-serialization
// without collapsing large integers or retyping numeric-looking text.
package sdk

import (
	"bytes"
	"encoding/json"
	"fmt"

	"rillweave/change"
	"rillweave/image"
	"rillweave/wire"
)

// Limit is the request body limit shared by the v1 and v2 commit entries.
const Limit = wire.MaxRequest

// Overflow describes a converted batch that no longer fits the shared
// request limit. The original v1 request remains replayable via
// POST /rill/v1/graphs/{name}/commit.
type Overflow struct {
	Bytes int
	Limit int
}

func (e *Overflow) Error() string {
	return fmt.Sprintf("converted v2 batch is %d bytes, exceeding the %d-byte request limit; "+
		"replay the original request via POST /rill/v1/graphs/{name}/commit", e.Bytes, e.Limit)
}

// Conversion is the outcome of converting one raw v1 commit request.
type Conversion struct {
	Body    []byte // v2 typed envelope request body
	V1Bytes int    // input size
	V2Bytes int    // converted size
	Limit   int    // shared request body limit
}

// Fits reports whether the converted body stays within the request limit.
func (c Conversion) Fits() bool {
	return c.V2Bytes <= c.Limit
}

// Overflow is nil when the converted body fits, and otherwise names the
// limit violation that sends the caller back to the v1 entry.
func (c Conversion) Overflow() *Overflow {
	if c.Fits() {
		return nil
	}
	return &Overflow{Bytes: c.V2Bytes, Limit: c.Limit}
}

// Convert decodes a raw v1 commit request body and renders its v2 typed
// envelope form. The decode is exact: integers never pass through a float.
// A request that v1 could never apply (unknown operation, mismatched
// payload) has no envelope form and is reported, leaving the v1 endpoint as
// the replay path. Callers must check Conversion.Fits before posting.
func Convert(raw []byte) (Conversion, error) {
	var batch change.Batch
	if err := image.ReadJSON(bytes.NewReader(raw), &batch); err != nil {
		return Conversion{}, fmt.Errorf("not a v1 commit request: %w", err)
	}
	envelope, err := change.Enclose(batch)
	if err != nil {
		return Conversion{}, fmt.Errorf("not convertible: %w", err)
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return Conversion{}, err
	}
	return Conversion{Body: body, V1Bytes: len(raw), V2Bytes: len(body), Limit: Limit}, nil
}
