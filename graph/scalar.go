package graph

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"unicode/utf8"
)

type Kind string

const (
	Text Kind = "text"
	Int  Kind = "int"
	Bool Kind = "bool"
)

type Scalar struct {
	Kind Kind
	Text string
	Int  int64
	Bool bool
}

func (k Kind) Valid() bool {
	return k == Text || k == Int || k == Bool
}

func (v Scalar) Valid() bool {
	switch v.Kind {
	case Text:
		return utf8.ValidString(v.Text) && len(v.Text) <= 1024 && v.Int == 0 && !v.Bool
	case Int:
		return v.Text == "" && !v.Bool
	case Bool:
		return v.Text == "" && v.Int == 0
	default:
		return false
	}
}

func (v Scalar) MarshalJSON() ([]byte, error) {
	if !v.Valid() {
		return nil, fmt.Errorf("invalid scalar")
	}
	switch v.Kind {
	case Text:
		return json.Marshal(v.Text)
	case Int:
		return strconv.AppendInt(nil, v.Int, 10), nil
	default:
		return strconv.AppendBool(nil, v.Bool), nil
	}
}

func (v *Scalar) UnmarshalJSON(raw []byte) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return fmt.Errorf("empty scalar")
	}
	var parsed Scalar
	switch {
	case raw[0] == '"':
		parsed.Kind = Text
		if err := json.Unmarshal(raw, &parsed.Text); err != nil {
			return err
		}
	case bytes.Equal(raw, []byte("true")), bytes.Equal(raw, []byte("false")):
		parsed.Kind, parsed.Bool = Bool, raw[0] == 't'
	default:
		parsed.Kind = Int
		var err error
		parsed.Int, err = strconv.ParseInt(string(raw), 10, 64)
		if err != nil {
			return fmt.Errorf("property must be text, boolean or signed 64-bit integer")
		}
	}
	if !parsed.Valid() {
		return fmt.Errorf("property text exceeds 1024 bytes")
	}
	*v = parsed
	return nil
}
