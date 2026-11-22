package compress

import (
	"slices"

	"rillweave/graph"
)

// Validate checks the internal consistency of a compressed document so that a
// malformed or hand-edited summary can never become an installed graph. It
// re-derives every referenced identity rather than trusting the statistics.
func Validate(d *Document) error {
	switch {
	case d == nil:
		return graph.Fail(graph.Invalid, "compressed document is empty")
	case !graph.ValidID(d.Name):
		return graph.Fail(graph.Invalid, "invalid compressed graph name")
	case !graph.ValidID(d.SourceName) || d.SourceRevision == 0:
		return graph.Fail(graph.Invalid, "compressed document lacks its source revision identity")
	case len(d.Clusters) == 0:
		return graph.Fail(graph.Invalid, "compressed document contains no clusters")
	case len(d.Endpoints) == 0 || len(d.Endpoints) > MaxEndpoints:
		return graph.Fail(graph.Invalid, "endpoint set must contain 1..%d origin vertex IDs", MaxEndpoints)
	case len(d.Kinds) > graph.MaxRules:
		return graph.Fail(graph.Invalid, "too many selected arc kinds")
	}
	if dup, ok := duplicate(d.Endpoints); ok {
		return graph.Fail(graph.Invalid, "endpoint %q is listed more than once", dup)
	}
	if dup, ok := duplicate(d.Kinds); ok {
		return graph.Fail(graph.Invalid, "arc kind %q is listed more than once", dup)
	}
	for _, kind := range d.Kinds {
		if !graph.ValidID(kind) {
			return graph.Fail(graph.Invalid, "invalid selected arc kind %q", kind)
		}
	}

	clusters := make(map[string]bool, len(d.Clusters))
	boundary := make(map[string]string)
	for _, cluster := range d.Clusters {
		if !graph.ValidID(cluster.ID) || clusters[cluster.ID] {
			return graph.Fail(graph.Invalid, "duplicate or invalid cluster %q", cluster.ID)
		}
		if cluster.MemberCount < len(cluster.Boundaries) || cluster.MemberCount == 0 {
			return graph.Fail(graph.Invalid, "cluster %q has an invalid member count", cluster.ID)
		}
		if dup, ok := duplicate(cluster.Boundaries); ok {
			return graph.Fail(graph.Invalid, "cluster %q lists boundary %q twice", cluster.ID, dup)
		}
		clusters[cluster.ID] = true
		for _, id := range cluster.Boundaries {
			if !graph.ValidID(id) {
				return graph.Fail(graph.Invalid, "invalid boundary vertex %q", id)
			}
			if owner, exists := boundary[id]; exists {
				return graph.Fail(graph.Invalid, "vertex %q appears in clusters %q and %q", id, owner, cluster.ID)
			}
			boundary[id] = cluster.ID
		}
	}
	for _, endpoint := range d.Endpoints {
		if _, ok := boundary[endpoint]; !ok {
			return graph.Fail(graph.Invalid, "endpoint %q is not a retained boundary vertex", endpoint)
		}
	}

	arcs := make(map[string]ArcRecord, len(d.ArcDict))
	for _, record := range d.ArcDict {
		if record.ID == "" || !graph.ValidID(record.ID) || record.Cost <= 0 || record.Cost > graph.MaxCost {
			return graph.Fail(graph.Invalid, "arc dictionary entry %q is invalid", record.ID)
		}
		if _, dup := arcs[record.ID]; dup {
			return graph.Fail(graph.Invalid, "arc dictionary lists %q twice", record.ID)
		}
		arcs[record.ID] = record
	}

	witnesses := make(map[string]Witness, len(d.Witnesses))
	seenSequence := make(map[string]bool, len(d.Witnesses))
	for _, witness := range d.Witnesses {
		if witness.ID == "" || !graph.ValidID(witness.ID) || len(witness.ArcIDs) == 0 ||
			witness.Hops != len(witness.ArcIDs) {
			return graph.Fail(graph.Invalid, "witness %q is invalid", witness.ID)
		}
		if _, dup := witnesses[witness.ID]; dup {
			return graph.Fail(graph.Invalid, "witness %q listed twice", witness.ID)
		}
		sequence := seqKey(witness.ArcIDs)
		if seenSequence[sequence] {
			return graph.Fail(graph.Invalid, "witness %q duplicates another arc sequence", witness.ID)
		}
		seenSequence[sequence] = true
		var total int64
		for _, arcID := range witness.ArcIDs {
			record, ok := arcs[arcID]
			if !ok {
				return graph.Fail(graph.Invalid, "witness %q references missing arc %q", witness.ID, arcID)
			}
			total += record.Cost
		}
		if total != witness.Cost {
			return graph.Fail(graph.Invalid, "witness %q cost does not match its arc records", witness.ID)
		}
		witnesses[witness.ID] = witness
	}

	seenCross := make(map[string]bool, len(d.CrossArcs))
	for _, arc := range d.CrossArcs {
		record, ok := arcs[arc.ArcID]
		if !ok {
			return graph.Fail(graph.Invalid, "cross arc %q is missing from the arc dictionary", arc.ArcID)
		}
		if seenCross[arc.ArcID] {
			return graph.Fail(graph.Invalid, "cross arc %q listed twice", arc.ArcID)
		}
		seenCross[arc.ArcID] = true
		if !clusters[arc.FromGroup] || !clusters[arc.ToGroup] || arc.FromGroup == arc.ToGroup {
			return graph.Fail(graph.Invalid, "cross arc %q joins invalid clusters", arc.ArcID)
		}
		if boundary[arc.From] != arc.FromGroup || boundary[arc.To] != arc.ToGroup ||
			record.From != arc.From || record.To != arc.To || record.Cost != arc.Cost ||
			record.Kind != arc.Kind {
			return graph.Fail(graph.Invalid, "cross arc %q disagrees with its arc record", arc.ArcID)
		}
		if len(d.Kinds) > 0 && !slices.Contains(d.Kinds, arc.Kind) {
			return graph.Fail(graph.Invalid, "cross arc %q has an unselected kind", arc.ArcID)
		}
	}

	pairs := make(map[[2]string]bool, len(d.Summaries))
	for _, edge := range d.Summaries {
		witness, ok := witnesses[edge.WitnessID]
		if !ok {
			return graph.Fail(graph.Invalid, "summary edge %q->%q references missing witness %q",
				edge.From, edge.To, edge.WitnessID)
		}
		if !graph.ValidID(edge.ID) || !clusters[edge.Cluster] || edge.Cost <= 0 ||
			edge.Hops != witness.Hops || edge.Cost != witness.Cost {
			return graph.Fail(graph.Invalid, "summary edge %q->%q in %q is invalid", edge.From, edge.To, edge.Cluster)
		}
		if boundary[edge.From] != edge.Cluster || boundary[edge.To] != edge.Cluster {
			return graph.Fail(graph.Invalid, "summary edge %q->%q leaves its cluster", edge.From, edge.To)
		}
		key := [2]string{edge.From, edge.To}
		if pairs[key] {
			return graph.Fail(graph.Invalid, "duplicate summary edge %q->%q", edge.From, edge.To)
		}
		pairs[key] = true
		vertex := edge.From
		for _, arcID := range witness.ArcIDs {
			record := arcs[arcID]
			if seenCross[arcID] {
				return graph.Fail(graph.Invalid, "witness for %q->%q reuses cross-group arc %q", edge.From, edge.To, arcID)
			}
			if record.From != vertex {
				return graph.Fail(graph.Invalid, "witness for %q->%q is not a contiguous walk at %q", edge.From, edge.To, arcID)
			}
			vertex = record.To
		}
		if vertex != edge.To {
			return graph.Fail(graph.Invalid, "witness for %q->%q does not replay to its declared end", edge.From, edge.To)
		}
	}
	return nil
}

func duplicate(values []string) (string, bool) {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if seen[value] {
			return value, true
		}
		seen[value] = true
	}
	return "", false
}
