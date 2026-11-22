package change

import (
	"cmp"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"maps"
	"slices"
	"strconv"
	"strings"

	"rillweave/graph"
)

const (
	DefaultDetailLimit = 100
	MaxDetailLimit     = 1000
	MaxDetailFields    = graph.MaxProps * 2
)

type Entities struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Updated []string `json:"updated"`
}

// Difference is the comparison result. Detail is nil unless the caller asked
// for per-entity field and relationship changes, so the summary shape is
// unchanged for existing consumers.
type Difference struct {
	From          uint64   `json:"from"`
	To            uint64   `json:"to"`
	Vertices      Entities `json:"vertices"`
	Arcs          Entities `json:"arcs"`
	SchemaChanged bool     `json:"schema_changed"`
	Detail        *Details `json:"detail,omitempty"`
}

// Query is the shaped comparison request. From and To select the same-graph
// revision pair; an empty handle means the current head. The remaining fields
// apply only when Detail is set.
type Query struct {
	From   string   `json:"from,omitempty"`
	To     string   `json:"to,omitempty"`
	Detail bool     `json:"detail,omitempty"`
	Fields []string `json:"fields,omitempty"`
	Limit  int      `json:"limit,omitempty"`
	Cursor string   `json:"cursor,omitempty"`
}

// DetailQuery controls the optional change entries. A nil Fields selects every
// property; an empty, non-nil Fields selects none. Cursor continues a page that
// was produced from the same revision pair.
type DetailQuery struct {
	Fields []string
	Limit  int
	Cursor string
}

// Page windows the change entries. Offset and Total count entries in the
// deterministic vertex-then-arc order, not fields.
type Page struct {
	Offset    int    `json:"offset"`
	Returned  int    `json:"returned"`
	Total     int    `json:"total"`
	Truncated bool   `json:"truncated"`
	Cursor    string `json:"cursor,omitempty"`
}

// TextChange always emits both sides. A null side means the value was absent;
// an empty string is an explicit value.
type TextChange struct {
	Before *string `json:"before"`
	After  *string `json:"after"`
}

// CostChange always emits both sides; null means the arc did not exist.
type CostChange struct {
	Before *int64 `json:"before"`
	After  *int64 `json:"after"`
}

// FieldChange always emits both sides. A null side is an unset property, which
// is distinct from an explicit empty, zero or false value.
type FieldChange struct {
	Name   string        `json:"name"`
	Before *graph.Scalar `json:"before"`
	After  *graph.Scalar `json:"after"`
}

// Entry is one vertex or arc change. Entity is "vertex" or "arc"; Change is
// "added", "removed" or "updated". Kind, From, To and Cost appear only when
// that structural part changed, so an arc whose ID kept its identity but moved
// its endpoints is recognizable without reading either graph again.
type Entry struct {
	Entity     string        `json:"entity"`
	ID         string        `json:"id"`
	Change     string        `json:"change"`
	Kind       *TextChange   `json:"kind,omitempty"`
	From       *TextChange   `json:"from,omitempty"`
	To         *TextChange   `json:"to,omitempty"`
	Cost       *CostChange   `json:"cost,omitempty"`
	Properties []FieldChange `json:"properties,omitempty"`
}

// Details carries one deterministic page of change entries derived from one
// resolved revision pair. Fingerprint identifies that pair; a cursor is bound to
// it so pages from different pairs cannot be stitched together.
type Details struct {
	Fingerprint string  `json:"fingerprint"`
	Page        Page    `json:"page"`
	Changes     []Entry `json:"changes"`
}

func Compare(from, to *graph.Graph) Difference {
	difference, _ := compare(from, to, nil)
	return difference
}

func CompareDetailed(from, to *graph.Graph, q DetailQuery) (Difference, error) {
	return compare(from, to, &q)
}

func compare(from, to *graph.Graph, q *DetailQuery) (Difference, error) {
	difference := Difference{
		From: from.Revision, To: to.Revision,
		Vertices:      diff(from.Vertices, to.Vertices, sameVertex),
		Arcs:          diff(from.Arcs, to.Arcs, sameArc),
		SchemaChanged: !sameSchema(from.Schema, to.Schema),
	}
	if q == nil {
		return difference, nil
	}
	detail, err := buildDetail(from, to, difference, q)
	if err != nil {
		return Difference{}, err
	}
	difference.Detail = &detail
	return difference, nil
}

func diff[V any](from, to map[string]V, equal func(V, V) bool) Entities {
	result := Entities{Added: []string{}, Removed: []string{}, Updated: []string{}}
	for _, id := range graph.Keys(to) {
		old, ok := from[id]
		if !ok {
			result.Added = append(result.Added, id)
		} else if !equal(old, to[id]) {
			result.Updated = append(result.Updated, id)
		}
	}
	for _, id := range graph.Keys(from) {
		if _, ok := to[id]; !ok {
			result.Removed = append(result.Removed, id)
		}
	}
	return result
}

func sameVertex(a, b graph.Vertex) bool {
	return a.Kind == b.Kind && maps.Equal(a.Properties, b.Properties)
}

func sameArc(a, b graph.Arc) bool {
	return a.Kind == b.Kind && a.From == b.From && a.To == b.To && a.Cost == b.Cost &&
		maps.Equal(a.Properties, b.Properties)
}

func sameSchema(a, b graph.Schema) bool {
	return maps.EqualFunc(a.Vertices, b.Vertices, func(a, b graph.VertexRule) bool {
		return maps.Equal(a.Properties, b.Properties)
	}) && maps.EqualFunc(a.Arcs, b.Arcs, func(a, b graph.ArcRule) bool {
		return a.From == b.From && a.To == b.To && maps.Equal(a.Properties, b.Properties)
	})
}

type changeKey struct {
	entity string
	id     string
}

type projection struct {
	all    bool
	fields map[string]bool
}

// selectFields validates the requested property names. nil means every
// property; a non-nil empty list means structural changes only.
func selectFields(from, to *graph.Graph, requested []string) (*projection, error) {
	if requested == nil {
		return &projection{all: true}, nil
	}
	if len(requested) > MaxDetailFields {
		return nil, graph.Fail(graph.Invalid, "at most %d detail fields may be requested", MaxDetailFields)
	}
	declared := schemaFields(from.Schema)
	for name := range schemaFields(to.Schema) {
		declared[name] = true
	}
	selected := make(map[string]bool, len(requested))
	for _, name := range requested {
		if !graph.ValidID(name) {
			return nil, graph.Fail(graph.Invalid, "invalid detail field %q", name)
		}
		if !declared[name] {
			return nil, graph.Fail(graph.Invalid, "property %q is not declared by either revision", name)
		}
		selected[name] = true
	}
	return &projection{fields: selected}, nil
}

func schemaFields(s graph.Schema) map[string]bool {
	names := make(map[string]bool)
	for _, rule := range s.Vertices {
		for name := range rule.Properties {
			names[name] = true
		}
	}
	for _, rule := range s.Arcs {
		for name := range rule.Properties {
			names[name] = true
		}
	}
	return names
}

func (p *projection) has(name string) bool {
	return p.all || p.fields[name]
}

func buildDetail(from, to *graph.Graph, summary Difference, q *DetailQuery) (Details, error) {
	selected, err := selectFields(from, to, q.Fields)
	if err != nil {
		return Details{}, err
	}
	limit := q.Limit
	if limit == 0 {
		limit = DefaultDetailLimit
	}
	if limit < 1 || limit > MaxDetailLimit {
		return Details{}, graph.Fail(graph.Invalid, "detail limit must be in [1,%d]", MaxDetailLimit)
	}

	fingerprint := Fingerprint(from, to)
	offset, err := startingOffset(fingerprint, q.Cursor)
	if err != nil {
		return Details{}, err
	}
	order := changeOrder(summary)
	total := len(order)
	if offset > total {
		return Details{}, graph.Fail(graph.Invalid, "detail cursor seeks past the %d change entries", total)
	}
	end := min(offset+limit, total)

	entries := make([]Entry, 0, end-offset)
	for _, key := range order[offset:end] {
		entries = append(entries, renderEntry(from, to, key, selected))
	}
	page := Page{Offset: offset, Returned: len(entries), Total: total}
	if end < total {
		page.Truncated = true
		page.Cursor = encodeCursor(fingerprint, end)
	}
	return Details{Fingerprint: fingerprint, Page: page, Changes: entries}, nil
}

// Fingerprint identifies the exact revision pair being compared. It is stable
// across pages and different for another pair of revisions.
func Fingerprint(from, to *graph.Graph) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{
		from.Name, strconv.FormatUint(from.Revision, 10),
		to.Name, strconv.FormatUint(to.Revision, 10),
	}, "\x00")))
	return hex.EncodeToString(sum[:16])
}

func encodeCursor(fingerprint string, offset int) string {
	raw := fingerprint + ":" + strconv.Itoa(offset)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(raw string) (string, int, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return "", 0, graph.Fail(graph.Invalid, "detail cursor is malformed")
	}
	fingerprint, rest, ok := strings.Cut(string(decoded), ":")
	if !ok || len(fingerprint) != 32 {
		return "", 0, graph.Fail(graph.Invalid, "detail cursor is malformed")
	}
	offset, err := strconv.Atoi(rest)
	if err != nil || offset < 0 {
		return "", 0, graph.Fail(graph.Invalid, "detail cursor is malformed")
	}
	return fingerprint, offset, nil
}

// startingOffset resolves continuation from a cursor. The cursor carries the
// fingerprint of the pair that produced the previous page, so a head that moved
// between requests is rejected instead of being stitched into the result.
func startingOffset(fingerprint, cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	bound, offset, err := decodeCursor(cursor)
	if err != nil {
		return 0, err
	}
	if bound != fingerprint {
		return 0, graph.Fail(graph.Conflict, "detail cursor belongs to a different revision pair")
	}
	return offset, nil
}

// changeOrder fixes paging order: vertices first, then arcs, each by ID. Every
// changed ID appears exactly once because the summary partitions the entities.
func changeOrder(summary Difference) []changeKey {
	keys := make([]changeKey, 0,
		len(summary.Vertices.Added)+len(summary.Vertices.Removed)+len(summary.Vertices.Updated)+
			len(summary.Arcs.Added)+len(summary.Arcs.Removed)+len(summary.Arcs.Updated))
	add := func(entity string, groups ...[]string) {
		for _, group := range groups {
			for _, id := range group {
				keys = append(keys, changeKey{entity, id})
			}
		}
	}
	add("vertex", summary.Vertices.Added, summary.Vertices.Removed, summary.Vertices.Updated)
	add("arc", summary.Arcs.Added, summary.Arcs.Removed, summary.Arcs.Updated)
	slices.SortFunc(keys, func(a, b changeKey) int {
		if a.entity != b.entity {
			return cmp.Compare(entityRank(a.entity), entityRank(b.entity))
		}
		return cmp.Compare(a.id, b.id)
	})
	return keys
}

func entityRank(entity string) int {
	if entity == "arc" {
		return 1
	}
	return 0
}

func renderEntry(from, to *graph.Graph, key changeKey, p *projection) Entry {
	entry := Entry{Entity: key.entity, ID: key.id, Properties: []FieldChange{}}
	if key.entity == "vertex" {
		old, hadOld := from.Vertices[key.id]
		next, hasNext := to.Vertices[key.id]
		entry.Change = classify(hadOld, hasNext)
		entry.Kind = textChange(hadOld, old.Kind, hasNext, next.Kind)
		entry.Properties = propertyChanges(old.Properties, next.Properties, p)
		return entry
	}
	old, hadOld := from.Arcs[key.id]
	next, hasNext := to.Arcs[key.id]
	entry.Change = classify(hadOld, hasNext)
	entry.Kind = textChange(hadOld, old.Kind, hasNext, next.Kind)
	entry.From = textChange(hadOld, old.From, hasNext, next.From)
	entry.To = textChange(hadOld, old.To, hasNext, next.To)
	entry.Cost = costChange(hadOld, old.Cost, hasNext, next.Cost)
	entry.Properties = propertyChanges(old.Properties, next.Properties, p)
	return entry
}

func classify(hadOld, hasNext bool) string {
	switch {
	case !hadOld:
		return "added"
	case !hasNext:
		return "removed"
	default:
		return "updated"
	}
}

func textChange(hadOld bool, old string, hasNext bool, next string) *TextChange {
	if hadOld && hasNext && old == next {
		return nil
	}
	change := &TextChange{}
	if hadOld {
		value := old
		change.Before = &value
	}
	if hasNext {
		value := next
		change.After = &value
	}
	return change
}

func costChange(hadOld bool, old int64, hasNext bool, next int64) *CostChange {
	if hadOld && hasNext && old == next {
		return nil
	}
	change := &CostChange{}
	if hadOld {
		value := old
		change.Before = &value
	}
	if hasNext {
		value := next
		change.After = &value
	}
	return change
}

func propertyChanges(oldProps, nextProps map[string]graph.Scalar, p *projection) []FieldChange {
	names := make([]string, 0, len(oldProps)+len(nextProps))
	for name := range oldProps {
		if p.has(name) {
			names = append(names, name)
		}
	}
	for name := range nextProps {
		if p.has(name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)

	changes := make([]FieldChange, 0, len(names))
	for _, name := range names {
		var before, after *graph.Scalar
		if value, ok := oldProps[name]; ok {
			copied := value
			before = &copied
		}
		if value, ok := nextProps[name]; ok {
			copied := value
			after = &copied
		}
		if before != nil && after != nil && *before == *after {
			continue
		}
		changes = append(changes, FieldChange{Name: name, Before: before, After: after})
	}
	return changes
}
