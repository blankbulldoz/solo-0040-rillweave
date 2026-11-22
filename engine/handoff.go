package engine

import (
	"rillweave/catalog"
	"rillweave/graph"
	"rillweave/handoff"
)

type HandoffReceipt struct {
	Source    Summary       `json:"source"`
	Installed Summary       `json:"installed"`
	Manifest  graph.Handoff `json:"manifest"`
}

// Handoff derives a region graph from the selected view and installs it under
// an unused name. All derivation, validation, capacity and image-size checks
// run before the durable write, so a rejected handoff changes neither the
// source graph nor any target namespace.
func (db *Database) Handoff(name, handle string, q handoff.Request) (HandoffReceipt, error) {
	if !graph.ValidID(q.Target) {
		return HandoffReceipt{}, graph.Fail(graph.Invalid, "invalid target graph name")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	s, err := db.resolve(name, handle)
	if err != nil {
		return HandoffReceipt{}, err
	}
	if db.heads[q.Target] != nil {
		return HandoffReceipt{}, graph.Fail(graph.Conflict, "graph %q already exists", q.Target)
	}
	if len(db.heads) == MaxGraphs {
		return HandoffReceipt{}, graph.Fail(graph.Budget, "graph namespace limit reached")
	}
	derived, err := handoff.Build(s.graph, s.adj, q)
	if err != nil {
		return HandoffReceipt{}, err
	}
	// The derived graph reuses the source schema and only complete vertices and
	// internal arcs, but validate it explicitly so capacity problems surface as
	// domain errors before any image or disk work begins.
	derived.Graph.Name = q.Target
	if err := catalog.Validate(derived.Graph); err != nil {
		return HandoffReceipt{}, err
	}
	installed, err := db.publish(derived.Graph)
	if err != nil {
		return HandoffReceipt{}, err
	}
	return HandoffReceipt{
		Source:    summary(s.graph),
		Installed: installed,
		Manifest:  derived.Manifest,
	}, nil
}

// HandoffManifest returns the provenance manifest of a handoff-derived graph
// at the selected revision. Ordinary graphs do not carry one.
func (db *Database) HandoffManifest(name, handle string) (ReadResult[graph.Handoff], error) {
	return query(db, name, handle, func(s *state) (graph.Handoff, error) {
		if s.graph.Handoff == nil {
			return graph.Handoff{}, graph.Fail(graph.Missing, "graph %q has no handoff manifest", name)
		}
		return *s.graph.Handoff, nil
	})
}
