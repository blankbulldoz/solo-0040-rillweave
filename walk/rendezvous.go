package walk

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"rillweave/graph"
	"rillweave/index"
)

// MaxSources bounds one side of a rendezvous. Every selected service carries
// one witness per device, so an unbounded device list would make the answer
// itself unbounded.
const MaxSources = 1024

// RendezvousGroup describes one side's devices and what that side accepts.
// An omitted max_cost means no limit below the one implied by max_hops; an
// explicit zero admits only a device that is itself the service.
type RendezvousGroup struct {
	Sources   []string        `json:"sources"`
	MaxHops   int             `json:"max_hops"`
	MaxCost   *int64          `json:"max_cost,omitempty"`
	Kinds     []string        `json:"kinds,omitempty"`
	Direction index.Direction `json:"direction,omitempty"`
}

type RendezvousRequest struct {
	Groups []RendezvousGroup `json:"groups"`
	Budget int               `json:"budget,omitempty"`
	Limit  int               `json:"limit,omitempty"`
}

type DeviceWitness struct {
	Source   string   `json:"source"`
	Cost     int64    `json:"cost"`
	Hops     int      `json:"hops"`
	Vertices []string `json:"vertices"`
	Arcs     []string `json:"arcs"`
}

type RendezvousSide struct {
	Group       int             `json:"group"`
	WorstCost   int64           `json:"worst_cost"`
	WorstHops   int             `json:"worst_hops"`
	WorstSource string          `json:"worst_source"`
	Devices     []DeviceWitness `json:"devices"`
}

type RendezvousCandidate struct {
	Vertex    string `json:"vertex"`
	WorstCost int64  `json:"worst_cost"`
	WorstHops int    `json:"worst_hops"`
}

type RendezvousSelection struct {
	Vertex     string           `json:"vertex"`
	WorstCost  int64            `json:"worst_cost"`
	WorstHops  int              `json:"worst_hops"`
	RankedFrom int              `json:"ranked_from"`
	Sides      []RendezvousSide `json:"sides"`
}

// RendezvousResult reports feasible services ordered by the cost faced by the
// most inconvenienced device on either side. Matched is the exact number of
// feasible services; Remaining and Truncated describe feasible services
// outside the returned window. A service is feasible only if every device on
// each side reaches it under that side's own kinds, hops and cost limit: it is
// not enough for one device per side to arrive. An exhausted budget is an
// error, never a short list that looks conclusive.
type RendezvousResult struct {
	Found      bool                  `json:"found"`
	Selected   *RendezvousSelection  `json:"selected,omitempty"`
	Candidates []RendezvousCandidate `json:"candidates"`
	Matched    int                   `json:"matched"`
	Remaining  int                   `json:"remaining"`
	Truncated  bool                  `json:"truncated"`
	Scanned    int                   `json:"scanned"`
}

type rendezvousSide struct {
	topology *topology
	group    int
	sources  []string
	maxHops  int
	maxCost  int64
}

type sideReach struct {
	cost   int64
	hops   int
	source string
}

func Rendezvous(g *graph.Graph, a *index.Adjacency, q RendezvousRequest) (RendezvousResult, error) {
	if len(q.Groups) != 2 {
		return RendezvousResult{}, graph.Fail(graph.Invalid, "rendezvous requires exactly two groups")
	}
	limit, err := Limit(q.Limit)
	if err != nil {
		return RendezvousResult{}, err
	}
	charge, err := newMeter(q.Budget)
	if err != nil {
		return RendezvousResult{}, err
	}
	sides, err := rendezvousSides(g, a, q, charge)
	if err != nil {
		return RendezvousResult{}, err
	}
	reachA, commonA, err := searchSide(sides[0], charge)
	if err != nil {
		return RendezvousResult{}, err
	}
	if len(commonA) == 0 {
		return emptyRendezvous(charge), nil
	}
	reachB, commonB, err := searchSide(sides[1], charge)
	if err != nil {
		return RendezvousResult{}, err
	}
	type ranked struct {
		vertex string
		cost   int64
		hops   int
	}
	list := make([]ranked, 0, len(commonA))
	for vertex := range commonA {
		if _, ok := commonB[vertex]; !ok {
			continue
		}
		a, b := reachA[vertex], reachB[vertex]
		// Each side is represented by its most inconvenienced device. A
		// candidate is ranked by the worse of the two sides, taken as a whole
		// so that the reported cost and hops stay one device's arrival.
		worst := a
		if worse(b, a) {
			worst = b
		}
		list = append(list, ranked{vertex, worst.cost, worst.hops})
	}
	if len(list) == 0 {
		return emptyRendezvous(charge), nil
	}
	slices.SortFunc(list, func(x, y ranked) int {
		if c := cmp.Compare(x.cost, y.cost); c != 0 {
			return c
		}
		if c := cmp.Compare(x.hops, y.hops); c != 0 {
			return c
		}
		return cmp.Compare(x.vertex, y.vertex)
	})
	shown := min(limit, len(list))
	candidates := make([]RendezvousCandidate, 0, shown)
	for _, row := range list[:shown] {
		candidates = append(candidates, RendezvousCandidate{row.vertex, row.cost, row.hops})
	}
	result := RendezvousResult{
		Found:      true,
		Candidates: candidates,
		Matched:    len(list),
		Remaining:  len(list) - shown,
		Truncated:  len(list) > shown,
	}
	selected := list[0]
	evidence := make([]RendezvousSide, len(sides))
	// Each side's own worst device, whether or not it is the one that sets the
	// candidate's rank, so a maintainer can see which group constrained the
	// answer. The rank itself is the worse of the two.
	rankedFrom := 0
	for i := range sides {
		side, err := evidenceFor(g, sides[i], selected.vertex, charge)
		if err != nil {
			return RendezvousResult{}, err
		}
		agg := reachA[selected.vertex]
		if i == 1 {
			agg = reachB[selected.vertex]
		}
		// The ranking ran without retaining labels. The delivered witnesses
		// must independently reproduce its distances; otherwise the selection
		// cannot be handed over for verification.
		if side.WorstCost != agg.cost || side.WorstHops != agg.hops || side.WorstSource != agg.source {
			return RendezvousResult{}, graph.Fail(graph.Unavailable,
				"rendezvous evidence for %q reports %q at cost %d hops %d, ranked %q at %d/%d",
				selected.vertex, side.WorstSource, side.WorstCost, side.WorstHops, agg.source, agg.cost, agg.hops)
		}
		if i == 1 && worse(reachB[selected.vertex], reachA[selected.vertex]) {
			rankedFrom = 1
		}
		evidence[i] = side
	}
	result.Selected = &RendezvousSelection{
		Vertex: selected.vertex, WorstCost: selected.cost, WorstHops: selected.hops,
		RankedFrom: rankedFrom, Sides: evidence,
	}
	result.Scanned = charge.scanned
	return result, nil
}

func emptyRendezvous(charge *meter) RendezvousResult {
	return RendezvousResult{Candidates: []RendezvousCandidate{}, Scanned: charge.scanned}
}

func rendezvousSides(g *graph.Graph, a *index.Adjacency, q RendezvousRequest, charge *meter) ([]rendezvousSide, error) {
	sides := make([]rendezvousSide, len(q.Groups))
	for i, grp := range q.Groups {
		if grp.MaxHops < 0 || grp.MaxHops > MaxHops {
			return nil, graph.Fail(graph.Invalid, "group %d: max_hops must be in [0,%d]", i, MaxHops)
		}
		maxCost, err := sideCost(grp.MaxCost)
		if err != nil {
			return nil, fmt.Errorf("group %d: %w", i, err)
		}
		if len(grp.Sources) == 0 || len(grp.Sources) > MaxSources {
			return nil, graph.Fail(graph.Invalid, "group %d requires 1..%d distinct sources", i, MaxSources)
		}
		seen := make(map[string]bool, len(grp.Sources))
		for _, source := range grp.Sources {
			if seen[source] {
				return nil, graph.Fail(graph.Invalid, "group %d lists source %q more than once", i, source)
			}
			seen[source] = true
			if err := requireVertex(g, source); err != nil {
				return nil, fmt.Errorf("group %d: %w", i, err)
			}
		}
		t, err := topologyShared(g, a, Selection{Kinds: grp.Kinds, Direction: grp.Direction}, charge)
		if err != nil {
			return nil, fmt.Errorf("group %d: %w", i, err)
		}
		sides[i] = rendezvousSide{t, i, grp.Sources, grp.MaxHops, maxCost}
	}
	return sides, nil
}

func sideCost(request *int64) (int64, error) {
	// No path can cost more than the largest hop count times the largest arc
	// cost, so a larger limit is rejected rather than silently clamped.
	ceiling := int64(MaxHops) * graph.MaxCost
	if request == nil {
		return ceiling, nil
	}
	if *request < 0 || *request > ceiling {
		return 0, graph.Fail(graph.Invalid, "max_cost must be in [0,%d]", ceiling)
	}
	return *request, nil
}

// searchSide walks every device of one side under that side's kinds, hops and
// cost limit. reach records, per vertex, the side's most inconvenienced device:
// the one whose cheapest arrival costs the most, then needs the most hops, then
// has the largest source ID. common keeps the vertices that every device
// reaches.
//
// Overlapping routes are common: once the intersection is empty it cannot grow
// again, so the remaining devices are not inspected. The intersection can
// still only be established exactly by searching each device, so a shared
// budget that runs out is reported, not hidden.
func searchSide(side rendezvousSide, charge *meter) (map[string]sideReach, map[string]bool, error) {
	reach := make(map[string]sideReach)
	common := make(map[string]bool)
	for i, source := range side.sources {
		best, err := distances(side.topology, source, side.maxHops, side.maxCost)
		if err != nil {
			return nil, nil, progress(err, "group %d (device %d of %d, %q) stopped searching; %d jointly reachable services found so far",
				side.group, i+1, len(side.sources), source, len(common))
		}
		if i == 0 {
			for vertex := range best {
				common[vertex] = true
			}
		} else {
			for vertex := range common {
				if _, ok := best[vertex]; !ok {
					delete(common, vertex)
				}
			}
		}
		for vertex, arrival := range best {
			candidate := sideReach{arrival.cost, arrival.hops, source}
			prior, seen := reach[vertex]
			if !seen || worse(candidate, prior) {
				reach[vertex] = candidate
			}
		}
		if len(common) == 0 && i+1 < len(side.sources) {
			return reach, common, nil
		}
	}
	return reach, common, nil
}

// distances is the ranking search: for each vertex it keeps the cheapest
// arrival under the side's rules, breaking ties by fewer hops. It labels by
// exact depth so a cheap prefix that spends hops the rest of the route needs
// is not discarded, but it keeps no arc sequence: the witnessed path is
// rebuilt once, for the selected service only. Every inspected candidate is
// charged, so the budget does not depend on which service is selected.
func distances(t *topology, source string, maxHops int, maxCost int64) (map[string]sideReach, error) {
	type arrival struct{ cost, hops int64 }
	best := map[string]arrival{source: {}}
	current := map[string]arrival{source: {}}
	for depth := 0; depth < maxHops && len(current) != 0; depth++ {
		next := make(map[string]arrival)
		err := t.visitEach(graph.Keys(current), func(vertex string, arc graph.Arc, target string) error {
			prefix := current[vertex]
			cost := prefix.cost + arc.Cost
			if cost > maxCost {
				return nil
			}
			candidate := arrival{cost, prefix.hops + 1}
			prior, seen := next[target]
			if !seen || candidate.cost < prior.cost || candidate.cost == prior.cost && candidate.hops < prior.hops {
				next[target] = candidate
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		for vertex, candidate := range next {
			if prior, seen := best[vertex]; !seen || candidate.cost < prior.cost || candidate.cost == prior.cost && candidate.hops < prior.hops {
				best[vertex] = candidate
			}
		}
		current = next
	}
	out := make(map[string]sideReach, len(best))
	for vertex, arrival := range best {
		out[vertex] = sideReach{cost: arrival.cost, hops: int(arrival.hops), source: source}
	}
	return out, nil
}

// worse orders two arrivals of one side by which device is more inconvenienced:
// the larger cost, then the larger hop count, then the larger source ID so that
// equal arrivals resolve deterministically.
func worse(a, b sideReach) bool {
	if a.cost != b.cost {
		return a.cost > b.cost
	}
	if a.hops != b.hops {
		return a.hops > b.hops
	}
	return a.source > b.source
}

// evidenceFor builds one witness per device with a single reverse search from
// the chosen service. A forward search per device would repeat the same
// overlapping routes; walking the side's arcs backwards reaches every device
// at once and yields the same minimum cost and hop count.
func evidenceFor(g *graph.Graph, side rendezvousSide, target string, charge *meter) (RendezvousSide, error) {
	reverse := side.topology.reversed()
	best, err := witnessFrom(reverse, target, side.maxHops, side.maxCost)
	if err != nil {
		return RendezvousSide{}, progress(err, "group %d evidence for service %q stopped at the shared budget", side.group, target)
	}
	evidence := RendezvousSide{Group: side.group, Devices: make([]DeviceWitness, 0, len(side.sources))}
	for _, source := range side.sources {
		label, ok := best[source]
		if !ok {
			return RendezvousSide{}, graph.Fail(graph.Unavailable,
				"rendezvous witness from %q to %q is missing after selection", source, target)
		}
		device := DeviceWitness{
			Source:   source,
			Cost:     label.cost,
			Hops:     len(label.arcs),
			Vertices: reversed(label.vertices),
			Arcs:     reversed(label.arcs),
		}
		if err := audit(g, side, target, device); err != nil {
			return RendezvousSide{}, err
		}
		worst := sideReach{device.Cost, device.Hops, device.Source}
		if evidence.WorstSource == "" || worse(worst, sideReach{evidence.WorstCost, evidence.WorstHops, evidence.WorstSource}) {
			evidence.WorstCost, evidence.WorstHops, evidence.WorstSource = device.Cost, device.Hops, source
		}
		evidence.Devices = append(evidence.Devices, device)
	}
	return evidence, nil
}

// audit replays a witness against the revision and the side's own rules. It is
// the check a maintainer can repeat: original arc identities, endpoints in the
// side's direction, allowed kinds, positive stored costs and both limits.
func audit(g *graph.Graph, side rendezvousSide, target string, w DeviceWitness) error {
	t := side.topology
	if len(w.Vertices) != len(w.Arcs)+1 || len(w.Vertices) == 0 {
		return graph.Fail(graph.Unavailable, "witness from %q to %q does not alternate %d vertices with %d arcs", w.Source, target, len(w.Vertices), len(w.Arcs))
	}
	if w.Vertices[0] != w.Source || w.Vertices[len(w.Vertices)-1] != target {
		return graph.Fail(graph.Unavailable, "witness from %q ends at %q instead of %q", w.Source, w.Vertices[len(w.Vertices)-1], target)
	}
	var cost int64
	for i, id := range w.Arcs {
		arc, ok := g.Arcs[id]
		if !ok {
			return graph.Fail(graph.Unavailable, "witness from %q names arc %q outside the chosen revision", w.Source, id)
		}
		if len(t.kinds) != 0 && !t.kinds[arc.Kind] {
			return graph.Fail(graph.Unavailable, "witness from %q uses arc kind %q the group does not accept", w.Source, arc.Kind)
		}
		from, to := w.Vertices[i], w.Vertices[i+1]
		if !connects(arc, from, to, t.direction) {
			return graph.Fail(graph.Unavailable, "witness from %q uses arc %q as %s -> %s against the group's direction", w.Source, id, from, to)
		}
		cost += arc.Cost
	}
	if cost != w.Cost {
		return graph.Fail(graph.Unavailable, "witness from %q reports cost %d but its arcs sum to %d", w.Source, w.Cost, cost)
	}
	if w.Hops > side.maxHops {
		return graph.Fail(graph.Unavailable, "witness from %q uses %d hops above the group limit %d", w.Source, w.Hops, side.maxHops)
	}
	if w.Cost > side.maxCost {
		return graph.Fail(graph.Unavailable, "witness from %q costs %d above the group limit %d", w.Source, w.Cost, side.maxCost)
	}
	return nil
}

func (t *topology) reversed() *topology {
	flipped := *t
	flipped.direction = reverseDirection(t.direction)
	return &flipped
}

func reverseDirection(d index.Direction) index.Direction {
	switch d {
	case index.In:
		return index.Out
	case index.Out:
		return index.In
	default:
		return index.Both
	}
}

func connects(arc graph.Arc, from, to string, d index.Direction) bool {
	forward := arc.From == from && arc.To == to
	backward := arc.From == to && arc.To == from
	switch d {
	case index.Out:
		return forward
	case index.In:
		return backward
	default:
		return forward || backward
	}
}

func reversed[T any](values []T) []T {
	out := make([]T, len(values))
	for i, value := range values {
		out[len(values)-1-i] = value
	}
	return out
}

// progress annotates a budget failure with how far the query got, so callers
// can tell "no service exists" from "services may remain unexcluded".
func progress(err error, format string, args ...any) error {
	var problem *graph.Problem
	if errors.As(err, &problem) && problem.Code == graph.Budget {
		return graph.Fail(graph.Budget, "%s: %s", fmt.Sprintf(format, args...), problem.Details)
	}
	return err
}

// witnessFrom is the path-bearing search used once per side, from the selected
// service backwards along the side's reversed arcs. It labels by exact depth so
// a cheap prefix that spends hops the rest of the route needs is not discarded,
// and keeps the lexicographically smallest arc sequence on a (cost, hops) tie.
// It must reach every device, so it expands the whole ball up to max_hops.
func witnessFrom(t *topology, target string, maxHops int, maxCost int64) (map[string]label, error) {
	best := map[string]label{target: {vertices: []string{target}}}
	current := map[string]label{target: {vertices: []string{target}}}
	for depth := 0; depth < maxHops && len(current) != 0; depth++ {
		next := make(map[string]label)
		for _, vertex := range graph.Keys(current) {
			prefix := current[vertex]
			err := t.visit(vertex, t.direction, func(arc graph.Arc, nextVertex string) error {
				cost := prefix.cost + arc.Cost
				if cost > maxCost {
					return nil
				}
				prior, seen := next[nextVertex]
				candidate := label{
					cost:     cost,
					arcs:     append(slices.Clone(prefix.arcs), arc.ID),
					vertices: append(slices.Clone(prefix.vertices), nextVertex),
				}
				if !seen || better(candidate, prior) {
					next[nextVertex] = candidate
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		for vertex, label := range next {
			if prior, seen := best[vertex]; !seen || better(label, prior) {
				best[vertex] = label
			}
		}
		current = next
	}
	return best, nil
}
