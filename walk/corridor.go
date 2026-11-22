package walk

import (
	"errors"
	"slices"

	"rillweave/graph"
	"rillweave/index"
)

// CorridorRequest exports the arc-level corridor between two vertices. An arc
// belongs to the corridor only when one single source-to-target walk contains
// it and satisfies both the hop and cost limits; walks may revisit vertices.
type CorridorRequest struct {
	From    string `json:"from"`
	To      string `json:"to"`
	MaxHops int    `json:"max_hops"`
	MaxCost int64  `json:"max_cost"`
	Samples int    `json:"samples,omitempty"`
	Selection
}

// CorridorArc is one included arc and, for the sampled prefix, a complete
// source-to-target walk whose replay proves the inclusion.
type CorridorArc struct {
	Arc     string   `json:"arc"`
	Witness []string `json:"witness,omitempty"`
}

const (
	// StatusComplete means every selected arc was classified.
	StatusComplete = "complete"
	// StatusTruncated means the expansion budget ran out during enumeration.
	// Included arcs stay provable; every other selected arc is undecided.
	StatusTruncated = "budget_exhausted"
)

type CorridorResult struct {
	Status    string        `json:"status"`
	Included  []CorridorArc `json:"included"`
	Excluded  []string      `json:"excluded"`
	Undecided []string      `json:"undecided"`
	Sampled   int           `json:"sampled"`
	Scanned   int           `json:"scanned"`
}

// travEdge is one permitted traversal step. In reverse selection the stored
// arc identity and cost are retained while the traversal endpoints are flipped.
type travEdge struct {
	arc graph.Arc
	src string
	dst string
}

type fPick struct {
	prev string
	arc  string
}

type bPick struct {
	next string
	arc  string
}

func Corridor(g *graph.Graph, a *index.Adjacency, q CorridorRequest) (CorridorResult, error) {
	if err := hops(q.MaxHops); err != nil {
		return CorridorResult{}, err
	}
	if q.MaxCost < 1 {
		return CorridorResult{}, graph.Fail(graph.Invalid, "max_cost must be a positive integer")
	}
	samples, err := Limit(q.Samples)
	if err != nil {
		return CorridorResult{}, err
	}
	t, err := topologyFor(g, a, q.Selection)
	if err != nil {
		return CorridorResult{}, err
	}
	for _, v := range []string{q.From, q.To} {
		if err := requireVertex(g, v); err != nil {
			return CorridorResult{}, err
		}
	}

	// Phase 1: enumerate the selected traversal edges, charging every adjacency
	// candidate (including kind rejections) against the expansion budget. When
	// the budget is exhausted the partial edge set is kept and classification
	// degrades to proven-inclusion versus undecided instead of failing the query.
	// Both incidence views are materialized because the backward labels expand
	// against the reversed traversal direction.
	out := make(map[string][]travEdge)
	into := make(map[string][]travEdge)
	byArc := make(map[string][]travEdge)
	truncated := false
	for _, v := range graph.Keys(g.Vertices) {
		err := t.visit(v, t.direction, func(arc graph.Arc, next string) error {
			// The posting vertex and the callback's next already encode the
			// traversal direction; a reversed step keeps the stored arc ID and
			// cost while its ends follow the adjacency visit.
			edge := travEdge{arc: arc, src: v, dst: next}
			out[v] = append(out[v], edge)
			into[next] = append(into[next], edge)
			byArc[arc.ID] = append(byArc[arc.ID], edge)
			return nil
		})
		if err != nil {
			var problem *graph.Problem
			if errors.As(err, &problem) && problem.Code == graph.Budget {
				truncated = true
				break
			}
			return CorridorResult{}, err
		}
	}

	// The universe is every arc that passes the kind selection, including arcs
	// the truncated enumeration never reached.
	universe := make([]string, 0, len(g.Arcs))
	for _, id := range graph.Keys(g.Arcs) {
		if len(t.kinds) == 0 || t.kinds[g.Arcs[id].Kind] {
			universe = append(universe, id)
		}
	}

	H, C := q.MaxHops, q.MaxCost

	// Phase 2a: forward labels. fd[h][v] is the cheapest exact-h-hop walk from
	// the source to v; strict relaxation over sorted vertices and arc-ordered
	// adjacency keeps the cheapest, lexicographically smallest witness stable.
	fd := make([]map[string]int64, H+1)
	fpick := make([]map[string]fPick, H+1)
	fd[0] = map[string]int64{q.From: 0}
	for h := 1; h <= H; h++ {
		fd[h] = make(map[string]int64)
		fpick[h] = make(map[string]fPick)
		for _, u := range graph.Keys(fd[h-1]) {
			prefix := fd[h-1][u]
			for _, e := range out[u] {
				cost := prefix + e.arc.Cost
				if prior, ok := fd[h][e.dst]; !ok || cost < prior {
					fd[h][e.dst] = cost
					fpick[h][e.dst] = fPick{prev: u, arc: e.arc.ID}
				}
			}
		}
	}

	// Phase 2b: backward labels. bd[k][v] is the cheapest exact-k-hop walk from
	// v to the target, retaining the stored arc cost in every selection mode.
	// Each label grows by prepending one incoming traversal edge to a walk that
	// already reaches the target in k-1 hops.
	bd := make([]map[string]int64, H+1)
	bpick := make([]map[string]bPick, H+1)
	bd[0] = map[string]int64{q.To: 0}
	for k := 1; k <= H; k++ {
		bd[k] = make(map[string]int64)
		bpick[k] = make(map[string]bPick)
		for _, w := range graph.Keys(bd[k-1]) {
			base := bd[k-1][w]
			for _, e := range into[w] {
				cost := e.arc.Cost + base
				if prior, ok := bd[k][e.src]; !ok || cost < prior {
					bd[k][e.src] = cost
					bpick[k][e.src] = bPick{next: w, arc: e.arc.ID}
				}
			}
		}
	}

	// minBD[k][v] is the cheapest walk from v to the target using at most k hops.
	minBD := make([]map[string]int64, H+1)
	best := make(map[string]int64)
	for k := 0; k <= H; k++ {
		for v, cost := range bd[k] {
			if prior, ok := best[v]; !ok || cost < prior {
				best[v] = cost
			}
		}
		minBD[k] = make(map[string]int64, len(best))
		for v, cost := range best {
			minBD[k][v] = cost
		}
	}

	result := CorridorResult{
		Status:    StatusComplete,
		Included:  []CorridorArc{},
		Excluded:  []string{},
		Undecided: []string{},
		Scanned:   t.scanned,
	}
	if truncated {
		result.Status = StatusTruncated
	}

	// Phase 3: classify each selected arc. The feasibility test always checks
	// the combined hop total and combined cost of one walk, so a prefix and a
	// suffix that separately fit the limits can never be stitched into evidence.
	for _, id := range universe {
		edge, hp, hs, ok := firstFeasible(id, H, C, byArc, fd, bd, minBD)
		if !ok {
			if truncated {
				result.Undecided = append(result.Undecided, id)
			} else {
				result.Excluded = append(result.Excluded, id)
			}
			continue
		}
		entry := CorridorArc{Arc: id}
		if result.Sampled < samples {
			entry.Witness = buildWitness(edge, hp, hs, fpick, bpick)
			result.Sampled++
		}
		result.Included = append(result.Included, entry)
	}
	return result, nil
}

// firstFeasible returns the first orientation and exact hop split for which the
// arc sits inside one walk meeting both limits. Unknown arcs (truncated
// enumeration) never qualify, and nothing can be provably excluded afterwards.
func firstFeasible(
	id string, H int, C int64,
	byArc map[string][]travEdge,
	fd, bd []map[string]int64, minBD []map[string]int64,
) (travEdge, int, int, bool) {
	for _, edge := range byArc[id] {
		for hp := 0; hp < H; hp++ {
			prefix, ok := fd[hp][edge.src]
			if !ok {
				continue
			}
			room := H - hp - 1
			cheapestSuffix, ok := minBD[room][edge.dst]
			if !ok || prefix+edge.arc.Cost+cheapestSuffix > C {
				continue
			}
			for hs := 0; hs <= room; hs++ {
				suffix, ok := bd[hs][edge.dst]
				if ok && prefix+edge.arc.Cost+suffix <= C {
					return edge, hp, hs, true
				}
			}
		}
	}
	return travEdge{}, 0, 0, false
}

// buildWitness replays the stored forward and backward choices into one arc ID
// sequence; the construction guarantees the hop and cost limits hold.
func buildWitness(
	edge travEdge, hp, hs int,
	fpick []map[string]fPick, bpick []map[string]bPick,
) []string {
	prefix := make([]string, 0, hp)
	vertex, k := edge.src, hp
	for k > 0 {
		pick := fpick[k][vertex]
		prefix = append(prefix, pick.arc)
		vertex = pick.prev
		k--
	}
	slices.Reverse(prefix)
	witness := make([]string, 0, hp+1+hs)
	witness = append(witness, prefix...)
	witness = append(witness, edge.arc.ID)
	vertex = edge.dst
	k = hs
	for k > 0 {
		pick := bpick[k][vertex]
		witness = append(witness, pick.arc)
		vertex = pick.next
		k--
	}
	return witness
}
