package change

import (
	"fmt"
	"slices"

	"rillweave/catalog"
	"rillweave/graph"
	"rillweave/index"
)

const MaxExpect = 64

// Match names a typed equality lookup, the same directory meaning as the
// lookup endpoint: a vertex kind, a declared property and a scalar value.
type Match struct {
	Kind     string       `json:"kind"`
	Property string       `json:"property"`
	Value    graph.Scalar `json:"value"`
}

// Expectation declares the complete match set a caller believes holds for one
// typed lookup. The set is given either explicitly as IDs — an empty array
// declares no matches — or as the canonical digest of the sorted IDs.
type Expectation struct {
	Match  Match     `json:"match"`
	IDs    *[]string `json:"ids,omitempty"`
	Digest *string   `json:"digest,omitempty"`
}

// verifyExpect checks every declared set against the revision accepting the
// edits. A declaration that is malformed or meaningless under the current
// schema is invalid; a well-formed declaration whose set no longer holds —
// including because new matches were inserted — fails the precondition.
func verifyExpect(g *graph.Graph, props *index.Postings, expect []Expectation) error {
	if len(expect) > MaxExpect {
		return graph.Fail(graph.Invalid, "a commit declares at most %d match sets", MaxExpect)
	}
	for n, e := range expect {
		if err := e.check(g, props); err != nil {
			return fmt.Errorf("expect %d: %w", n, err)
		}
	}
	return nil
}

func (e Expectation) check(g *graph.Graph, props *index.Postings) error {
	if (e.IDs == nil) == (e.Digest == nil) {
		return graph.Fail(graph.Invalid, "declare exactly one of ids or digest")
	}
	if err := catalog.LookupRule(g.Schema, e.Match.Kind, e.Match.Property, e.Match.Value); err != nil {
		return err
	}
	actual := props.Match(e.Match.Kind, e.Match.Property, e.Match.Value)
	if e.Digest != nil {
		if !digestForm(*e.Digest) {
			return graph.Fail(graph.Invalid, "digest must be 64 lowercase hexadecimal characters")
		}
		if *e.Digest != index.DigestIDs(actual) {
			return changed(e.Match)
		}
		return nil
	}
	claimed := slices.Clone(*e.IDs)
	slices.Sort(claimed)
	for i := 1; i < len(claimed); i++ {
		if claimed[i-1] == claimed[i] {
			return graph.Fail(graph.Invalid, "duplicate id %q in the declared set", claimed[i])
		}
	}
	if !slices.Equal(claimed, actual) {
		return changed(e.Match)
	}
	return nil
}

func changed(m Match) error {
	return graph.Fail(graph.Precondition,
		"declared match set for %q property %q does not hold at the accepting revision", m.Kind, m.Property)
}

func digestForm(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}
