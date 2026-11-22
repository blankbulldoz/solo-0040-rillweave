package walk

import (
	"rillweave/graph"
	"rillweave/index"
)

// A batch serves several point-level adjacency questions from one pinned
// revision under one arc inspection allowance. The single-vertex Adjacent read
// is unchanged; a batch exists because inspecting a hot hub first can otherwise
// consume the whole page before a cheap vertex is even looked at.
//
// Fairness contract, in request order:
//
//   - Every query is guaranteed a minimum inspection opportunity of
//     budget/queries candidates, and the page must be large enough to give each
//     query at least one. That floor is reported per item as guaranteed, so a
//     caller can re-derive what the read promised. A query that finishes inside
//     its reservation leaves the rest in the page.
//   - Once every query has had its reservation, the unspent remainder is handed
//     out in round-robin one-candidate slices to queries that are still working,
//     so a finished query never holds budget away from the others.
//   - Only a query whose whole incidence stream was inspected reports
//     complete:true with matched as an exact total. truncated then says whether
//     that exact total exceeded the item's output window, mirroring the
//     single-vertex read. A query stopped by the page allowance reports
//     complete:false and truncated:false: its matched value is a lower bound
//     observed so far, never a truncated subtotal presented as a total.
//
// Candidate accounting is identical to a single read: a candidate is charged
// before kind filtering, and a loop in a bidirectional query is charged once.
const (
	MinBatchQueries = 1
	MaxBatchQueries = 256
)

type BatchQuery struct {
	Vertex    string          `json:"vertex"`
	Direction index.Direction `json:"direction,omitempty"`
	Kinds     []string        `json:"kinds,omitempty"`
}

type BatchRequest struct {
	Queries []BatchQuery `json:"queries"`
	Limit   int          `json:"limit,omitempty"`
	Budget  int          `json:"budget,omitempty"`
}

type BatchItem struct {
	Vertex     string          `json:"vertex"`
	Direction  index.Direction `json:"direction"`
	Kinds      []string        `json:"kinds,omitempty"`
	Arcs       []graph.Arc     `json:"arcs"`
	Matched    int             `json:"matched"`
	Truncated  bool            `json:"truncated"`
	Complete   bool            `json:"complete"`
	Scanned    int             `json:"scanned"`
	Guaranteed int             `json:"guaranteed"`
}

type BatchResult struct {
	Items    []BatchItem `json:"items"`
	Scanned  int         `json:"scanned"`
	Budget   int         `json:"budget"`
	Complete bool        `json:"complete"`
}

type batchItem struct {
	query      BatchQuery
	direction  index.Direction
	kinds      map[string]bool
	limit      int
	cursor     *index.Cursor
	arcs       []graph.Arc
	matched    int
	scanned    int
	guaranteed int
	complete   bool
}

func Batch(g *graph.Graph, a *index.Adjacency, q BatchRequest) (BatchResult, error) {
	if len(q.Queries) < MinBatchQueries || len(q.Queries) > MaxBatchQueries {
		return BatchResult{}, graph.Fail(graph.Invalid, "queries must contain %d..%d items", MinBatchQueries, MaxBatchQueries)
	}
	limit, err := Limit(q.Limit)
	if err != nil {
		return BatchResult{}, err
	}
	budget := q.Budget
	if budget == 0 {
		budget = MaxBudget
	}
	if budget < 1 || budget > MaxBudget {
		return BatchResult{}, graph.Fail(graph.Invalid, "invalid expansion budget")
	}
	if budget < len(q.Queries) {
		return BatchResult{}, graph.Fail(graph.Invalid,
			"budget %d cannot give each of %d queries one inspection", budget, len(q.Queries))
	}
	items := make([]*batchItem, len(q.Queries))
	for n, question := range q.Queries {
		d, kinds, err := selection(g, question)
		if err != nil {
			return BatchResult{}, err
		}
		if err := requireVertex(g, question.Vertex); err != nil {
			return BatchResult{}, err
		}
		items[n] = &batchItem{
			query:     BatchQuery{question.Vertex, d, kinds},
			direction: d, kinds: kindSet(kinds), limit: limit,
			arcs: []graph.Arc{},
		}
	}
	reserve(items, budget)
	b := &batcher{adj: a, items: items, budget: budget}
	b.reserveAll()
	b.roundRobin()
	result := BatchResult{Items: make([]BatchItem, len(items)), Scanned: b.scanned, Budget: budget, Complete: true}
	for n, it := range items {
		result.Items[n] = BatchItem{
			Vertex: it.query.Vertex, Direction: it.direction, Kinds: it.query.Kinds,
			Arcs: it.arcs, Matched: it.matched, Truncated: it.complete && it.matched > it.limit,
			Complete: it.complete, Scanned: it.scanned, Guaranteed: it.guaranteed,
		}
		if !it.complete {
			result.Complete = false
		}
	}
	return result, nil
}

// reserve fixes the verifiable minimum opportunity for each item. Every item is
// promised at least one inspection because Batch rejects a page too small to
// cover one candidate per query.
func reserve(items []*batchItem, budget int) {
	for _, it := range items {
		it.guaranteed = budget / len(items)
	}
}

type batcher struct {
	adj     *index.Adjacency
	items   []*batchItem
	budget  int
	scanned int
}

// reserveAll spends the reservations in rounds of at most one candidate per
// item, never letting an item exceed its own share. Interleaving matters: the
// first query in the request cannot take its whole reservation before the last
// query has been inspected once. A query that completes inside its share simply
// stops appearing in later rounds, returning the unused part to the page for the
// round-robin phase.
func (b *batcher) reserveAll() {
	for {
		progressed := false
		for _, it := range b.items {
			if it.complete || it.scanned >= it.guaranteed {
				continue
			}
			b.take(it)
			progressed = true
		}
		if !progressed {
			return
		}
	}
}

// roundRobin advances each unfinished item at most once per round. A hot hub
// therefore cannot take two candidates while another item is still waiting, and
// any page allowance left over from completed reservations is shared evenly.
func (b *batcher) roundRobin() {
	for b.scanned < b.budget {
		advanced := false
		for _, it := range b.items {
			if it.complete {
				continue
			}
			b.take(it)
			advanced = true
		}
		if !advanced {
			return
		}
	}
}

// take charges one candidate inspection before filtering, exactly like a single
// read, and records the arc in original incidence order while the window lasts.
// An item that has already run out of page allowance is left untouched so it
// cannot report a truncated observation as a finished count.
func (b *batcher) take(it *batchItem) {
	if b.scanned == b.budget {
		return
	}
	if it.cursor == nil {
		it.cursor = b.adj.Cursor(it.query.Vertex, it.direction)
	}
	arc, _, ok := it.cursor.Next()
	if !ok {
		it.complete = true
		return
	}
	b.scanned++
	it.scanned++
	if len(it.kinds) == 0 || it.kinds[arc.Kind] {
		it.matched++
		if len(it.arcs) < it.limit {
			it.arcs = append(it.arcs, arc)
		}
	}
	if !it.cursor.Pending() {
		it.complete = true
	}
}

// selection validates one query's direction and kind filter against the pinned
// catalog before any budget is spent, so invalid input never yields a prefix.
func selection(g *graph.Graph, q BatchQuery) (index.Direction, []string, error) {
	d := q.Direction
	if d == "" {
		d = index.Out
	}
	if d != index.Out && d != index.In && d != index.Both {
		return "", nil, graph.Fail(graph.Invalid, "direction must be out, in or both")
	}
	if len(q.Kinds) > graph.MaxRules {
		return "", nil, graph.Fail(graph.Invalid, "invalid arc-kind count")
	}
	for _, kind := range q.Kinds {
		if _, ok := g.Schema.Arcs[kind]; !ok {
			return "", nil, graph.Fail(graph.Invalid, "unknown arc kind %q", kind)
		}
	}
	return d, q.Kinds, nil
}

func kindSet(kinds []string) map[string]bool {
	if len(kinds) == 0 {
		return nil
	}
	set := make(map[string]bool, len(kinds))
	for _, kind := range kinds {
		set[kind] = true
	}
	return set
}
