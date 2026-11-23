package change

import (
	"fmt"
	"strconv"

	"rillweave/graph"
)

// EnvelopeBatch is the /rill/v2 form of a commit request. Every 64-bit
// integer travels as a canonical decimal string, so a request survives a
// browser's JSON parse and re-serialization, where numbers collapse to
// float64 and adjacent large integers stop being distinct. Arc costs stay
// plain numbers: valid costs never exceed graph.MaxCost, which float64
// represents exactly.
type EnvelopeBatch struct {
	Base  string     `json:"base"`
	Edits []Envelope `json:"edits"`
}

// Envelope is one typed operation. Exactly one key must be present, so an
// operation can never disagree with its payload the way a v1 "op" edit can.
type Envelope struct {
	PutVertex  *VertexEnvelope `json:"put_vertex,omitempty"`
	PutArc     *ArcEnvelope    `json:"put_arc,omitempty"`
	DropVertex *string         `json:"drop_vertex,omitempty"`
	DropArc    *string         `json:"drop_arc,omitempty"`
	SetSchema  *graph.Schema   `json:"set_schema,omitempty"`
}

// ScalarEnvelope is a typed property value. Exactly one key must be present;
// "int" carries the canonical decimal form of a signed 64-bit integer, and
// "text" keeps text that looks like a number distinct from one.
type ScalarEnvelope struct {
	Text *string `json:"text,omitempty"`
	Int  *string `json:"int,omitempty"`
	Bool *bool   `json:"bool,omitempty"`
}

type VertexEnvelope struct {
	ID         string                    `json:"id"`
	Kind       string                    `json:"kind"`
	Properties map[string]ScalarEnvelope `json:"properties,omitempty"`
}

type ArcEnvelope struct {
	ID         string                    `json:"id"`
	Kind       string                    `json:"kind"`
	From       string                    `json:"from"`
	To         string                    `json:"to"`
	Cost       int64                     `json:"cost"`
	Properties map[string]ScalarEnvelope `json:"properties,omitempty"`
}

// Batch decodes an envelope batch into the domain batch shared with v1, so
// both endpoints commit through the same preparation and validation path.
func (b EnvelopeBatch) Batch() (Batch, error) {
	base, err := parseUint64("base", b.Base)
	if err != nil {
		return Batch{}, err
	}
	edits := make([]Edit, len(b.Edits))
	for n, envelope := range b.Edits {
		edit, err := envelope.edit()
		if err != nil {
			return Batch{}, fmt.Errorf("edit %d: %w", n, err)
		}
		edits[n] = edit
	}
	return Batch{Base: base, Edits: edits}, nil
}

func (e Envelope) edit() (Edit, error) {
	count := 0
	var edit Edit
	if v := e.PutVertex; v != nil {
		count++
		vertex, err := v.vertex()
		if err != nil {
			return Edit{}, err
		}
		edit = Edit{Op: "put_vertex", Vertex: &vertex}
	}
	if a := e.PutArc; a != nil {
		count++
		arc, err := a.arc()
		if err != nil {
			return Edit{}, err
		}
		edit = Edit{Op: "put_arc", Arc: &arc}
	}
	if e.DropVertex != nil {
		count++
		edit = Edit{Op: "drop_vertex", ID: e.DropVertex}
	}
	if e.DropArc != nil {
		count++
		edit = Edit{Op: "drop_arc", ID: e.DropArc}
	}
	if e.SetSchema != nil {
		count++
		edit = Edit{Op: "set_schema", Schema: e.SetSchema}
	}
	if count != 1 {
		return Edit{}, graph.Fail(graph.Invalid, "exactly one typed operation is required")
	}
	return edit, nil
}

func (e VertexEnvelope) vertex() (graph.Vertex, error) {
	props, err := properties(e.Properties)
	if err != nil {
		return graph.Vertex{}, err
	}
	return graph.Vertex{ID: e.ID, Kind: e.Kind, Properties: props}, nil
}

func (e ArcEnvelope) arc() (graph.Arc, error) {
	props, err := properties(e.Properties)
	if err != nil {
		return graph.Arc{}, err
	}
	return graph.Arc{
		ID: e.ID, Kind: e.Kind, From: e.From, To: e.To, Cost: e.Cost, Properties: props,
	}, nil
}

func properties(env map[string]ScalarEnvelope) (map[string]graph.Scalar, error) {
	if env == nil {
		return nil, nil
	}
	props := make(map[string]graph.Scalar, len(env))
	for _, name := range graph.Keys(env) {
		value, err := env[name].scalar()
		if err != nil {
			return nil, fmt.Errorf("property %q: %w", name, err)
		}
		props[name] = value
	}
	return props, nil
}

func (e ScalarEnvelope) scalar() (graph.Scalar, error) {
	count := 0
	var value graph.Scalar
	if e.Text != nil {
		count++
		value = graph.Scalar{Kind: graph.Text, Text: *e.Text}
	}
	if e.Int != nil {
		count++
		n, err := parseInt64("int property", *e.Int)
		if err != nil {
			return graph.Scalar{}, err
		}
		value = graph.Scalar{Kind: graph.Int, Int: n}
	}
	if e.Bool != nil {
		count++
		value = graph.Scalar{Kind: graph.Bool, Bool: *e.Bool}
	}
	if count != 1 {
		return graph.Scalar{}, graph.Fail(graph.Invalid, "exactly one typed value is required")
	}
	if !value.Valid() {
		return graph.Scalar{}, graph.Fail(graph.Invalid, "property text exceeds 1024 bytes")
	}
	return value, nil
}

func parseInt64(what, raw string) (int64, error) {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || strconv.FormatInt(n, 10) != raw {
		return 0, graph.Fail(graph.Invalid, "%s must be a canonical signed 64-bit integer", what)
	}
	return n, nil
}

func parseUint64(what, raw string) (uint64, error) {
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || strconv.FormatUint(n, 10) != raw {
		return 0, graph.Fail(graph.Invalid, "%s must be a canonical unsigned 64-bit integer", what)
	}
	return n, nil
}

// Enclose converts a decoded v1 batch into its v2 typed envelope form. An
// edit whose operation and payload disagree, and therefore could never be
// applied by v1, has no envelope form and is reported rather than silently
// dropped.
func Enclose(batch Batch) (EnvelopeBatch, error) {
	edits := make([]Envelope, len(batch.Edits))
	for n, edit := range batch.Edits {
		envelope, err := encloseEdit(edit)
		if err != nil {
			return EnvelopeBatch{}, fmt.Errorf("edit %d: %w", n, err)
		}
		edits[n] = envelope
	}
	return EnvelopeBatch{Base: strconv.FormatUint(batch.Base, 10), Edits: edits}, nil
}

func encloseEdit(edit Edit) (Envelope, error) {
	if !edit.onePayload() {
		return Envelope{}, graph.Fail(graph.Invalid, "edit does not carry exactly one payload")
	}
	switch {
	case edit.Op == "put_vertex" && edit.Vertex != nil:
		return Envelope{PutVertex: wrapVertex(*edit.Vertex)}, nil
	case edit.Op == "put_arc" && edit.Arc != nil:
		return Envelope{PutArc: wrapArc(*edit.Arc)}, nil
	case edit.Op == "set_schema" && edit.Schema != nil:
		return Envelope{SetSchema: edit.Schema}, nil
	case edit.Op == "drop_vertex" && edit.ID != nil:
		id := *edit.ID
		return Envelope{DropVertex: &id}, nil
	case edit.Op == "drop_arc" && edit.ID != nil:
		id := *edit.ID
		return Envelope{DropArc: &id}, nil
	default:
		return Envelope{}, graph.Fail(graph.Invalid, "operation %q does not match its payload", edit.Op)
	}
}

func wrapVertex(v graph.Vertex) *VertexEnvelope {
	return &VertexEnvelope{ID: v.ID, Kind: v.Kind, Properties: wrapProperties(v.Properties)}
}

func wrapArc(a graph.Arc) *ArcEnvelope {
	return &ArcEnvelope{
		ID: a.ID, Kind: a.Kind, From: a.From, To: a.To, Cost: a.Cost,
		Properties: wrapProperties(a.Properties),
	}
}

func wrapProperties(props map[string]graph.Scalar) map[string]ScalarEnvelope {
	if props == nil {
		return nil
	}
	wrapped := make(map[string]ScalarEnvelope, len(props))
	for name, value := range props {
		wrapped[name] = Wrap(value)
	}
	return wrapped
}

// Wrap renders a domain scalar as its typed envelope. Integers become
// canonical decimal strings; text and booleans keep their JSON types.
func Wrap(v graph.Scalar) ScalarEnvelope {
	switch v.Kind {
	case graph.Text:
		text := v.Text
		return ScalarEnvelope{Text: &text}
	case graph.Int:
		raw := strconv.FormatInt(v.Int, 10)
		return ScalarEnvelope{Int: &raw}
	default:
		boolean := v.Bool
		return ScalarEnvelope{Bool: &boolean}
	}
}
