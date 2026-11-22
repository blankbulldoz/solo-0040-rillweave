package walk

import (
	"errors"
	"slices"

	"rillweave/graph"
	"rillweave/index"
)

const (
	MaxMatrixSide  = 64
	MaxMatrixCells = 4096
)

// Cell statuses. A route cell carries a minimum cost; a no-route cell was
// fully searched under the same hop, direction and kind constraints; an
// undetermined cell was not decided before the shared work budget ran out.
const (
	Route        = "route"
	NoRoute      = "no_route"
	Undetermined = "undetermined"
)

type CellRef struct {
	Entry string `json:"entry"`
	Exit  string `json:"exit"`
}

type MatrixRequest struct {
	Entries []string  `json:"entries"`
	Exits   []string  `json:"exits"`
	MaxHops int       `json:"max_hops"`
	Witness []CellRef `json:"witness,omitempty"`
	Selection
}

type CellWitness struct {
	Vertices []string `json:"vertices"`
	Arcs     []string `json:"arcs"`
}

type Cell struct {
	Entry   string       `json:"entry"`
	Exit    string       `json:"exit"`
	Status  string       `json:"status"`
	Cost    *int64       `json:"cost,omitempty"`
	Witness *CellWitness `json:"witness,omitempty"`
}

type MatrixSummary struct {
	Routes       int `json:"routes"`
	NoRoute      int `json:"no_route"`
	Undetermined int `json:"undetermined"`
}

type MatrixResult struct {
	Entries  []string      `json:"entries"`
	Exits    []string      `json:"exits"`
	Cells    []Cell        `json:"cells"`
	Summary  MatrixSummary `json:"summary"`
	Searches int           `json:"searches"`
	Scanned  int           `json:"scanned"`
	Complete bool          `json:"complete"`
}

type cellKey struct {
	entry, exit string
}

// Matrix resolves every entry/exit pair against one selected view under one
// shared candidate budget. It runs one forward search per entry when entries
// are scarcer and one backward search per exit otherwise, so the repeated
// side of the batch is explored once rather than once per pair. Every search
// uses the same hop limit, direction and arc-kind selection, and a cell is
// only called no_route after the search covering it finished every allowed
// depth. Cells left uncovered by an exhausted budget are undetermined; they
// are never reported as no_route.
func Matrix(g *graph.Graph, a *index.Adjacency, q MatrixRequest) (MatrixResult, error) {
	if err := hops(q.MaxHops); err != nil {
		return MatrixResult{}, err
	}
	t, err := topologyFor(g, a, q.Selection)
	if err != nil {
		return MatrixResult{}, err
	}
	if err := matrixShape(q); err != nil {
		return MatrixResult{}, err
	}
	for _, id := range q.Entries {
		if err := requireVertex(g, id); err != nil {
			return MatrixResult{}, err
		}
	}
	for _, id := range q.Exits {
		if err := requireVertex(g, id); err != nil {
			return MatrixResult{}, err
		}
	}
	witness, err := matrixWitness(q)
	if err != nil {
		return MatrixResult{}, err
	}
	labels := make([]map[int]label, len(q.Entries))
	for i := range labels {
		labels[i] = make(map[int]label)
	}
	decided := make([]bool, len(q.Entries)*len(q.Exits))
	searches, complete := 0, true
	if len(q.Entries) <= len(q.Exits) {
		for ei, entry := range q.Entries {
			found, err := t.forwardLabels(entry, q.MaxHops)
			searches++
			if err != nil {
				if !exhausted(err) {
					return MatrixResult{}, err
				}
				complete = false
				break
			}
			for xi, exit := range q.Exits {
				if foundLabel, ok := found[exit]; ok {
					labels[ei][xi] = foundLabel
				}
				decided[ei*len(q.Exits)+xi] = true
			}
		}
	} else {
		for xi, exit := range q.Exits {
			found, err := t.reverseLabels(exit, q.MaxHops)
			searches++
			if err != nil {
				if !exhausted(err) {
					return MatrixResult{}, err
				}
				complete = false
				break
			}
			for ei, entry := range q.Entries {
				if foundLabel, ok := found[entry]; ok {
					labels[ei][xi] = foundLabel
				}
				decided[ei*len(q.Exits)+xi] = true
			}
		}
	}
	result := MatrixResult{
		Entries:  slices.Clone(q.Entries),
		Exits:    slices.Clone(q.Exits),
		Cells:    make([]Cell, 0, len(decided)),
		Searches: searches,
		Scanned:  t.scanned,
		Complete: complete,
	}
	for ei, entry := range q.Entries {
		for xi, exit := range q.Exits {
			cell := Cell{Entry: entry, Exit: exit}
			foundLabel, ok := labels[ei][xi]
			switch {
			case !decided[ei*len(q.Exits)+xi] && entry != exit:
				cell.Status = Undetermined
				result.Summary.Undetermined++
			case !decided[ei*len(q.Exits)+xi]:
				// Costs are strictly positive, so the empty path is the
				// cheapest route from a vertex to itself even without a
				// search; like a single-pair query this needs no budget.
				cell.Status = Route
				result.Summary.Routes++
				foundLabel = label{vertices: []string{entry}, arcs: []string{}}
				ok = true
			case ok:
				cell.Status = Route
				result.Summary.Routes++
			default:
				cell.Status = NoRoute
				result.Summary.NoRoute++
			}
			if ok {
				cost := foundLabel.cost
				cell.Cost = &cost
				if witness[cellKey{entry, exit}] {
					cell.Witness = &CellWitness{
						Vertices: slices.Clone(foundLabel.vertices),
						Arcs:     slices.Clone(foundLabel.arcs),
					}
				}
			}
			result.Cells = append(result.Cells, cell)
		}
	}
	return result, nil
}

// forwardLabels returns the best bounded label for every vertex reachable
// from source, including the zero-hop label for the source itself.
func (t *topology) forwardLabels(source string, maxHops int) (map[string]label, error) {
	best := map[string]label{source: {vertices: []string{source}, arcs: []string{}}}
	current := map[string]label{source: best[source]}
	for depth := 0; depth < maxHops && len(current) != 0; depth++ {
		next := make(map[string]label)
		for _, vertex := range graph.Keys(current) {
			prefix := current[vertex]
			err := t.visit(vertex, t.direction, func(arc graph.Arc, target string) error {
				candidate := label{
					cost:     prefix.cost + arc.Cost,
					arcs:     append(slices.Clone(prefix.arcs), arc.ID),
					vertices: append(slices.Clone(prefix.vertices), target),
				}
				if prior, seen := next[target]; !seen || better(candidate, prior) {
					next[target] = candidate
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		for vertex, foundLabel := range next {
			if prior, seen := best[vertex]; !seen || better(foundLabel, prior) {
				best[vertex] = foundLabel
			}
		}
		current = next
	}
	return best, nil
}

// reverseLabels searches the selected relation backwards from target. Labels
// keep their ordinary forward orientation, so costs, arc identities and the
// cost/depth/arc-sequence ordering match a forward single-pair search.
func (t *topology) reverseLabels(target string, maxHops int) (map[string]label, error) {
	best := map[string]label{target: {vertices: []string{target}, arcs: []string{}}}
	current := map[string]label{target: best[target]}
	for depth := 0; depth < maxHops && len(current) != 0; depth++ {
		next := make(map[string]label)
		for _, vertex := range graph.Keys(current) {
			suffix := current[vertex]
			err := t.visit(vertex, t.reverse(), func(arc graph.Arc, predecessor string) error {
				candidate := label{
					cost:     suffix.cost + arc.Cost,
					arcs:     append([]string{arc.ID}, suffix.arcs...),
					vertices: append([]string{predecessor}, suffix.vertices...),
				}
				if prior, seen := next[predecessor]; !seen || better(candidate, prior) {
					next[predecessor] = candidate
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		for vertex, foundLabel := range next {
			if prior, seen := best[vertex]; !seen || better(foundLabel, prior) {
				best[vertex] = foundLabel
			}
		}
		current = next
	}
	return best, nil
}

func (t *topology) reverse() index.Direction {
	switch t.direction {
	case index.Out:
		return index.In
	case index.In:
		return index.Out
	default:
		return index.Both
	}
}

func matrixShape(q MatrixRequest) error {
	if len(q.Entries) == 0 || len(q.Exits) == 0 {
		return graph.Fail(graph.Invalid, "matrix requires at least one entry and one exit")
	}
	if len(q.Entries) > MaxMatrixSide || len(q.Exits) > MaxMatrixSide {
		return graph.Fail(graph.Invalid, "matrix allows at most %d entries and %d exits", MaxMatrixSide, MaxMatrixSide)
	}
	if len(q.Entries)*len(q.Exits) > MaxMatrixCells {
		return graph.Fail(graph.Invalid, "matrix allows at most %d cells", MaxMatrixCells)
	}
	if repeated(q.Entries) != "" {
		return graph.Fail(graph.Invalid, "duplicate matrix entry %q", repeated(q.Entries))
	}
	if repeated(q.Exits) != "" {
		return graph.Fail(graph.Invalid, "duplicate matrix exit %q", repeated(q.Exits))
	}
	return nil
}

func repeated(ids []string) string {
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			return id
		}
		seen[id] = true
	}
	return ""
}

func matrixWitness(q MatrixRequest) (map[cellKey]bool, error) {
	if len(q.Witness) == 0 {
		return nil, nil
	}
	entries := make(map[string]bool, len(q.Entries))
	for _, id := range q.Entries {
		entries[id] = true
	}
	exits := make(map[string]bool, len(q.Exits))
	for _, id := range q.Exits {
		exits[id] = true
	}
	witness := make(map[cellKey]bool, len(q.Witness))
	for _, ref := range q.Witness {
		key := cellKey{ref.Entry, ref.Exit}
		if !entries[ref.Entry] || !exits[ref.Exit] {
			return nil, graph.Fail(graph.Invalid, "witness cell %q -> %q is not in this matrix", ref.Entry, ref.Exit)
		}
		if witness[key] {
			return nil, graph.Fail(graph.Invalid, "witness cell %q -> %q is repeated", ref.Entry, ref.Exit)
		}
		witness[key] = true
	}
	return witness, nil
}

func exhausted(err error) bool {
	var problem *graph.Problem
	return errors.As(err, &problem) && problem.Code == graph.Budget
}
