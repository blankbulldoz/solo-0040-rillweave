package walk

import (
	"errors"
	"slices"

	"rillweave/graph"
	"rillweave/index"
)

// MaxCircuitTargets bounds the number of audited arcs in one batch and
// MaxCircuitLengths the hop counts requested per arc.
const (
	MaxCircuitTargets = 100
	MaxCircuitLengths = 32
)

const maxCost64 = int64(^uint64(0) >> 1)

// Circuit outcome labels. "found" and "no_solution" are exhaustive verdicts;
// "incomplete" means the inspection budget was spent before either was proven.
const (
	CircuitFound      = "found"
	CircuitNoSolution = "no_solution"
	CircuitIncomplete = "incomplete"
)

// CircuitRequest audits concrete arc identities at caller-chosen exact hop
// counts. The same kind selection applies to every candidate; empty kinds
// means all kinds. Forward traversal only.
type CircuitRequest struct {
	Kinds   []string        `json:"kinds,omitempty"`
	Budget  int             `json:"budget,omitempty"`
	Targets []CircuitTarget `json:"targets"`
}

type CircuitTarget struct {
	Arc     string `json:"arc"`
	Lengths []int  `json:"lengths"`
}

// CircuitCheck is one arc/length verdict. A witness lists every arc in walk
// order, beginning with the audited arc.
type CircuitCheck struct {
	Arc     string   `json:"arc"`
	Length  int      `json:"length"`
	Status  string   `json:"status"`
	Cost    int64    `json:"cost,omitempty"`
	Arcs    []string `json:"arcs,omitempty"`
	Scanned int      `json:"scanned"`
}

type CircuitResult struct {
	Checks  []CircuitCheck `json:"checks"`
	Scanned int            `json:"scanned"`
}

// Circuits replays each named arc and, for every requested exact length,
// returns the cheapest closed trail that starts with it. All pairs share one
// inspection allowance, so an exhausted budget leaves later pairs incomplete
// rather than failing the whole batch.
func Circuits(g *graph.Graph, a *index.Adjacency, q CircuitRequest) (CircuitResult, error) {
	t, err := topologyFor(g, a, Selection{Kinds: q.Kinds, Budget: q.Budget})
	if err != nil {
		return CircuitResult{}, err
	}
	if len(q.Targets) == 0 || len(q.Targets) > MaxCircuitTargets {
		return CircuitResult{}, graph.Fail(graph.Invalid, "target count must be in [1,%d]", MaxCircuitTargets)
	}
	result := CircuitResult{Checks: []CircuitCheck{}}
	s := &circuitSearch{t: t}
	for _, target := range q.Targets {
		arc, ok := g.Arcs[target.Arc]
		if !ok {
			return CircuitResult{}, graph.Fail(graph.Invalid, "arc %q is not in this revision", target.Arc)
		}
		if len(target.Lengths) == 0 || len(target.Lengths) > MaxCircuitLengths {
			return CircuitResult{}, graph.Fail(graph.Invalid, "length count for arc %q must be in [1,%d]", target.Arc, MaxCircuitLengths)
		}
		lengths := slices.Clone(target.Lengths)
		slices.Sort(lengths)
		lengths = slices.Compact(lengths)
		if len(lengths) != len(target.Lengths) || lengths[0] < 1 || lengths[len(lengths)-1] > MaxHops {
			return CircuitResult{}, graph.Fail(graph.Invalid, "lengths for arc %q must be unique values in [1,%d]", target.Arc, MaxHops)
		}
		for _, length := range lengths {
			result.Checks = append(result.Checks, s.check(arc, length))
		}
	}
	result.Scanned = t.scanned
	return result, nil
}

type circuitSearch struct {
	t *topology
}

func (s *circuitSearch) check(arc graph.Arc, length int) CircuitCheck {
	check := CircuitCheck{Arc: arc.ID, Length: length, Scanned: s.t.scanned}
	// The audited arc itself must pass the shared kind selection.
	if len(s.t.kinds) != 0 && !s.t.kinds[arc.Kind] {
		check.Status = CircuitNoSolution
		check.Scanned = s.t.scanned
		return check
	}
	if length == 1 {
		// A one-hop closed trail is a self-loop and uses its real cost.
		if arc.From == arc.To {
			check.Status = CircuitFound
			check.Cost = arc.Cost
			check.Arcs = []string{arc.ID}
		} else {
			check.Status = CircuitNoSolution
		}
		check.Scanned = s.t.scanned
		return check
	}
	used := map[string]bool{arc.ID: true}
	best := label{cost: maxCost64}
	incomplete := s.dfs(arc.To, arc.From, length-1, arc.Cost, []string{arc.ID}, used, &best)
	check.Scanned = s.t.scanned
	switch {
	case best.arcs != nil:
		check.Status = CircuitFound
		check.Cost = best.cost
		check.Arcs = best.arcs
	case incomplete:
		check.Status = CircuitIncomplete
	default:
		check.Status = CircuitNoSolution
	}
	return check
}

// dfs extends a trail that currently sits at vertex from, carrying prefixCost
// and prefixArcs, until it reaches origin in exactly remaining steps without
// reusing an arc ID. It reports whether the budget was spent before the search
// space was exhausted.
func (s *circuitSearch) dfs(from, origin string, remaining int, prefixCost int64, prefixArcs []string, used map[string]bool, best *label) bool {
	if remaining == 0 {
		if from == origin {
			candidate := label{cost: prefixCost, arcs: slices.Clone(prefixArcs)}
			if best.arcs == nil || better(candidate, *best) {
				*best = candidate
			}
		}
		return false
	}
	exhausted := false
	err := s.t.visit(from, index.Out, func(arc graph.Arc, next string) error {
		if used[arc.ID] {
			return nil
		}
		cost := prefixCost + arc.Cost
		// Exact branch-and-bound on accumulated cost. An equal-cost branch is
		// kept at the leaf so a lexicographically smaller arc-ID sequence can
		// still win the tie; with more steps left positive costs make an equal
		// prefix unable to finish better.
		if best.arcs != nil && (cost > best.cost || remaining > 1 && cost == best.cost) {
			return nil
		}
		used[arc.ID] = true
		incomplete := s.dfs(next, origin, remaining-1, cost, append(prefixArcs, arc.ID), used, best)
		delete(used, arc.ID)
		if incomplete {
			exhausted = true
			return errStop
		}
		return nil
	})
	if exhausted {
		return true
	}
	return isBudget(err)
}

// errStop unwinds a successful search once a nested frame spent the budget.
var errStop = graph.Fail(graph.Budget, "circuit search stopped")

func isBudget(err error) bool {
	var problem *graph.Problem
	return errors.As(err, &problem) && problem.Code == graph.Budget
}
