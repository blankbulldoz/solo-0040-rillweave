package change

import (
	"fmt"
	"slices"

	"rillweave/catalog"
	"rillweave/graph"
)

// Read contracts are registered consumers of one graph condition. A contract
// is meaningful only if the older ("from") revision can serve it, so Invalid
// input reports a stale or malformed registration instead of an audit result.
const (
	MaxContracts        = 256
	DefaultWitnessLimit = 5
	MaxWitnessLimit     = 50
)

const (
	ContractUnaffected   = "unaffected"
	ContractChanged      = "changed"
	ContractInapplicable = "inapplicable"
)

const (
	GuaranteeRequired = "required"
	GuaranteeUnique   = "unique"
)

// VertexSelector is a point query by vertex kind, property and typed equality.
// Requires names catalog guarantees the consumer reads through.
type VertexSelector struct {
	Kind     string        `json:"kind"`
	Property string        `json:"property"`
	Value    *graph.Scalar `json:"value"`
	Requires []string      `json:"requires,omitempty"`
}

// ArcSelector selects original arcs by arc kind and the expected kinds of
// their start and end vertices.
type ArcSelector struct {
	Kind string `json:"kind"`
	From string `json:"from"`
	To   string `json:"to"`
}

type ReadContract struct {
	ID     string          `json:"id"`
	Vertex *VertexSelector `json:"vertex,omitempty"`
	Arc    *ArcSelector    `json:"arc,omitempty"`
}

type GuaranteeChange struct {
	Name string `json:"name"`
	From bool   `json:"from"`
	To   bool   `json:"to"`
	Lost bool   `json:"lost"`
}

// MatchDifference carries complete identity sets. It is never derived from a
// bounded lookup or adjacency window.
type MatchDifference struct {
	Added     []string `json:"added"`
	Removed   []string `json:"removed"`
	FromTotal int      `json:"from_total"`
	ToTotal   int      `json:"to_total"`
}

// Witness is one original record behind an added or removed match. Witnesses
// are a bounded sample for spot checks; the complete identity lists are in
// MatchDifference.
type Witness struct {
	ID     string        `json:"id"`
	Vertex *graph.Vertex `json:"vertex,omitempty"`
	Arc    *graph.Arc    `json:"arc,omitempty"`
}

type Witnesses struct {
	Limit            int       `json:"limit"`
	Added            []Witness `json:"added"`
	Removed          []Witness `json:"removed"`
	AddedTruncated   bool      `json:"added_truncated"`
	RemovedTruncated bool      `json:"removed_truncated"`
}

type ContractResult struct {
	ID         string            `json:"id"`
	Vertex     *VertexSelector   `json:"vertex,omitempty"`
	Arc        *ArcSelector      `json:"arc,omitempty"`
	Status     string            `json:"status"`
	Reason     string            `json:"reason,omitempty"`
	Guarantees []GuaranteeChange `json:"guarantees"`
	Matches    *MatchDifference  `json:"matches,omitempty"`
	Witnesses  *Witnesses        `json:"witnesses,omitempty"`
}

type ContractSummary struct {
	Contracts           int `json:"contracts"`
	Unaffected          int `json:"unaffected"`
	Changed             int `json:"changed"`
	Inapplicable        int `json:"inapplicable"`
	EvaluatedConditions int `json:"evaluated_conditions"`
	SharedRegistrations int `json:"shared_registrations"`
}

// ContractComparison binds every conclusion to the two resolved revisions.
// FromHandle and ToHandle echo the caller's pins (empty selected the head).
type ContractComparison struct {
	From       uint64           `json:"from"`
	To         uint64           `json:"to"`
	FromHandle string           `json:"from_handle,omitempty"`
	ToHandle   string           `json:"to_handle,omitempty"`
	Contracts  []ContractResult `json:"contracts"`
	Summary    ContractSummary  `json:"summary"`
}

type vertexKey struct {
	kind, property string
	value          graph.Scalar
}

type arcKey struct {
	kind, from, to string
}

type conditionResult struct {
	fromIDs    []string
	toIDs      []string
	applicable bool
	reason     string
}

func CompareContracts(from, to *graph.Graph, contracts []ReadContract, witnessLimit int) (ContractComparison, error) {
	limit, err := witnessesLimit(witnessLimit)
	if err != nil {
		return ContractComparison{}, err
	}
	if len(contracts) == 0 || len(contracts) > MaxContracts {
		return ContractComparison{}, graph.Fail(graph.Invalid, "contracts requires 1..%d registrations", MaxContracts)
	}
	seen := make(map[string]bool, len(contracts))
	for n, contract := range contracts {
		if contract.ID == "" || len(contract.ID) > 64 {
			return ContractComparison{}, graph.Fail(graph.Invalid, "contract %d requires an id of 1..64 bytes", n)
		}
		if seen[contract.ID] {
			return ContractComparison{}, graph.Fail(graph.Invalid, "contract id %q is registered twice", contract.ID)
		}
		seen[contract.ID] = true
		if (contract.Vertex == nil) == (contract.Arc == nil) {
			return ContractComparison{}, graph.Fail(graph.Invalid, "contract %q requires exactly one vertex or arc selector", contract.ID)
		}
		if contract.Vertex != nil {
			if err := checkVertexPremise(from, contract.ID, *contract.Vertex); err != nil {
				return ContractComparison{}, err
			}
		} else if err := checkArcPremise(from, contract.ID, *contract.Arc); err != nil {
			return ContractComparison{}, err
		}
	}

	vertices := make(map[vertexKey]conditionResult)
	arcs := make(map[arcKey]conditionResult)
	result := ContractComparison{
		From: from.Revision, To: to.Revision,
		Contracts: make([]ContractResult, 0, len(contracts)),
	}
	for _, contract := range contracts {
		var row ContractResult
		if contract.Vertex != nil {
			row = compareVertex(from, to, contract, vertices, limit)
		} else {
			row = compareArc(from, to, contract, arcs, limit)
		}
		switch row.Status {
		case ContractUnaffected:
			result.Summary.Unaffected++
		case ContractInapplicable:
			result.Summary.Inapplicable++
		default:
			result.Summary.Changed++
		}
		result.Contracts = append(result.Contracts, row)
	}
	result.Summary.Contracts = len(contracts)
	result.Summary.EvaluatedConditions = len(vertices) + len(arcs)
	result.Summary.SharedRegistrations = len(contracts) - result.Summary.EvaluatedConditions
	return result, nil
}

func witnessesLimit(request int) (int, error) {
	if request == 0 {
		return DefaultWitnessLimit, nil
	}
	if request < 1 || request > MaxWitnessLimit {
		return 0, graph.Fail(graph.Invalid, "witness_limit must be in [1,%d]", MaxWitnessLimit)
	}
	return request, nil
}

// checkVertexPremise enforces that the registration describes something the
// older revision actually serves, including the guarantees it claims.
func checkVertexPremise(from *graph.Graph, id string, sel VertexSelector) error {
	if sel.Kind == "" || sel.Property == "" {
		return graph.Fail(graph.Invalid, "contract %q requires vertex kind and property", id)
	}
	if sel.Value == nil {
		return graph.Fail(graph.Invalid, "contract %q requires a typed equality value", id)
	}
	if err := catalog.LookupRule(from.Schema, sel.Kind, sel.Property, *sel.Value); err != nil {
		return graph.Fail(graph.Invalid, "contract %q is not executable on revision %d: %v", id, from.Revision, err)
	}
	rule := from.Schema.Vertices[sel.Kind].Properties[sel.Property]
	seen := make(map[string]bool, len(sel.Requires))
	for _, name := range sel.Requires {
		if name != GuaranteeRequired && name != GuaranteeUnique {
			return graph.Fail(graph.Invalid, "contract %q names unknown guarantee %q", id, name)
		}
		if seen[name] {
			return graph.Fail(graph.Invalid, "contract %q repeats guarantee %q", id, name)
		}
		seen[name] = true
		held := rule.Required
		if name == GuaranteeUnique {
			held = rule.Unique
		}
		if !held {
			return graph.Fail(graph.Invalid, "contract %q depends on a %s guarantee that revision %d does not provide", id, name, from.Revision)
		}
	}
	return nil
}

func checkArcPremise(from *graph.Graph, id string, sel ArcSelector) error {
	if sel.Kind == "" || sel.From == "" || sel.To == "" {
		return graph.Fail(graph.Invalid, "contract %q requires arc kind and expected endpoint kinds", id)
	}
	rule, ok := from.Schema.Arcs[sel.Kind]
	if !ok {
		return graph.Fail(graph.Invalid, "contract %q selects unknown arc kind %q on revision %d", id, sel.Kind, from.Revision)
	}
	if rule.From != sel.From || rule.To != sel.To {
		return graph.Fail(graph.Invalid, "contract %q expects %q -> %q but revision %d registers %q -> %q",
			id, sel.From, sel.To, from.Revision, rule.From, rule.To)
	}
	return nil
}

func compareVertex(from, to *graph.Graph, contract ReadContract, cache map[vertexKey]conditionResult, limit int) ContractResult {
	sel := contract.Vertex
	row := ContractResult{ID: contract.ID, Vertex: sel, Guarantees: []GuaranteeChange{}}
	key := vertexKey{sel.Kind, sel.Property, *sel.Value}
	evaluated, ok := cache[key]
	if !ok {
		evaluated = evaluateVertex(from, to, key)
		cache[key] = evaluated
	}
	if !evaluated.applicable {
		row.Status = ContractInapplicable
		row.Reason = evaluated.reason
		return row
	}
	oldRule := from.Schema.Vertices[sel.Kind].Properties[sel.Property]
	newRule := to.Schema.Vertices[sel.Kind].Properties[sel.Property]
	row.Guarantees = guaranteeChanges(sel.Requires, oldRule, newRule)
	row.Matches = matchDifference(evaluated.fromIDs, evaluated.toIDs)
	row.Witnesses = witnesses(row.Matches, from, to, false, limit)
	if len(row.Guarantees) == 0 && len(row.Matches.Added) == 0 && len(row.Matches.Removed) == 0 {
		row.Status = ContractUnaffected
	} else {
		row.Status = ContractChanged
	}
	return row
}

func evaluateVertex(from, to *graph.Graph, key vertexKey) conditionResult {
	result := conditionResult{fromIDs: []string{}, toIDs: []string{}}
	if _, ok := to.Schema.Vertices[key.kind]; !ok {
		result.reason = fmt.Sprintf("vertex kind %q is absent in revision %d", key.kind, to.Revision)
		return result
	}
	rule, ok := to.Schema.Vertices[key.kind].Properties[key.property]
	if !ok {
		result.reason = fmt.Sprintf("property %q on vertex kind %q is absent in revision %d", key.property, key.kind, to.Revision)
		return result
	}
	if rule.Kind != key.value.Kind {
		result.reason = fmt.Sprintf("property %q on vertex kind %q requires %s in revision %d, not %s",
			key.property, key.kind, rule.Kind, to.Revision, key.value.Kind)
		return result
	}
	result.applicable = true
	result.fromIDs = matchVertices(from, key)
	result.toIDs = matchVertices(to, key)
	return result
}

func compareArc(from, to *graph.Graph, contract ReadContract, cache map[arcKey]conditionResult, limit int) ContractResult {
	sel := contract.Arc
	row := ContractResult{ID: contract.ID, Arc: sel, Guarantees: []GuaranteeChange{}}
	key := arcKey{sel.Kind, sel.From, sel.To}
	evaluated, ok := cache[key]
	if !ok {
		evaluated = evaluateArc(from, to, key)
		cache[key] = evaluated
	}
	if !evaluated.applicable {
		row.Status = ContractInapplicable
		row.Reason = evaluated.reason
		return row
	}
	row.Matches = matchDifference(evaluated.fromIDs, evaluated.toIDs)
	row.Witnesses = witnesses(row.Matches, from, to, true, limit)
	if len(row.Matches.Added) == 0 && len(row.Matches.Removed) == 0 {
		row.Status = ContractUnaffected
	} else {
		row.Status = ContractChanged
	}
	return row
}

func evaluateArc(from, to *graph.Graph, key arcKey) conditionResult {
	result := conditionResult{fromIDs: []string{}, toIDs: []string{}}
	rule, ok := to.Schema.Arcs[key.kind]
	if !ok {
		result.reason = fmt.Sprintf("arc kind %q is absent in revision %d", key.kind, to.Revision)
		return result
	}
	if rule.From != key.from || rule.To != key.to {
		result.reason = fmt.Sprintf("arc kind %q endpoints changed from %s -> %s in revision %d to %s -> %s",
			key.kind, key.from, key.to, to.Revision, rule.From, rule.To)
		return result
	}
	result.applicable = true
	result.fromIDs = matchArcs(from, key)
	result.toIDs = matchArcs(to, key)
	return result
}

func guaranteeChanges(requires []string, oldRule, newRule graph.PropertyRule) []GuaranteeChange {
	changes := make([]GuaranteeChange, 0, len(requires))
	for _, name := range requires {
		oldHeld, newHeld := oldRule.Required, newRule.Required
		if name == GuaranteeUnique {
			oldHeld, newHeld = oldRule.Unique, newRule.Unique
		}
		if oldHeld != newHeld {
			changes = append(changes, GuaranteeChange{name, oldHeld, newHeld, oldHeld && !newHeld})
		}
	}
	return changes
}

func matchDifference(fromIDs, toIDs []string) *MatchDifference {
	added := missing(toIDs, fromIDs)
	removed := missing(fromIDs, toIDs)
	return &MatchDifference{
		Added: added, Removed: removed,
		FromTotal: len(fromIDs), ToTotal: len(toIDs),
	}
}

// missing returns the sorted members of ids absent from other. Both inputs are
// complete, sorted identity sets.
func missing(ids, other []string) []string {
	result := make([]string, 0)
	for _, id := range ids {
		if _, ok := slices.BinarySearch(other, id); !ok {
			result = append(result, id)
		}
	}
	return result
}

func matchVertices(g *graph.Graph, key vertexKey) []string {
	ids := make([]string, 0)
	for _, id := range graph.Keys(g.Vertices) {
		v := g.Vertices[id]
		if v.Kind != key.kind {
			continue
		}
		if value, ok := v.Properties[key.property]; ok && value == key.value {
			ids = append(ids, id)
		}
	}
	return ids
}

func matchArcs(g *graph.Graph, key arcKey) []string {
	ids := make([]string, 0)
	for _, id := range graph.Keys(g.Arcs) {
		a := g.Arcs[id]
		if a.Kind != key.kind {
			continue
		}
		source, hasSource := g.Vertices[a.From]
		target, hasTarget := g.Vertices[a.To]
		if !hasSource || !hasTarget || source.Kind != key.from || target.Kind != key.to {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

func witnesses(diff *MatchDifference, from, to *graph.Graph, arc bool, limit int) *Witnesses {
	added, addedCut := sampleRecords(to, diff.Added, arc, limit)
	removed, removedCut := sampleRecords(from, diff.Removed, arc, limit)
	return &Witnesses{
		Limit: limit, Added: added, Removed: removed,
		AddedTruncated: addedCut, RemovedTruncated: removedCut,
	}
}

func sampleRecords(g *graph.Graph, ids []string, arc bool, limit int) ([]Witness, bool) {
	n := min(limit, len(ids))
	sample := make([]Witness, 0, n)
	for _, id := range ids[:n] {
		w := Witness{ID: id}
		if arc {
			record := g.Arcs[id]
			w.Arc = &record
		} else {
			record := g.Vertices[id]
			w.Vertex = &record
		}
		sample = append(sample, w)
	}
	return sample, len(ids) > limit
}
