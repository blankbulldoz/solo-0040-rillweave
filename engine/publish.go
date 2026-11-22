package engine

import (
	"log"

	"rillweave/catalog"
	"rillweave/change"
	"rillweave/graph"
	"rillweave/image"
)

type Summary struct {
	Name     string `json:"name"`
	Revision uint64 `json:"revision"`
	Vertices int    `json:"vertices"`
	Arcs     int    `json:"arcs"`
}

func summary(g *graph.Graph) Summary {
	return Summary{g.Name, g.Revision, len(g.Vertices), len(g.Arcs)}
}

func (db *Database) Create(name string, s graph.Schema) (Summary, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.mayInstall(name); err != nil {
		return Summary{}, err
	}
	g := graph.New(name, s)
	if err := catalog.Validate(g); err != nil {
		return Summary{}, err
	}
	return db.publish(g)
}

func (db *Database) Commit(name string, batch change.Batch) (Summary, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	head, err := db.resolve(name, "")
	if err != nil {
		return Summary{}, err
	}
	if batch.Rebase != "" && batch.Rebase != change.RebaseSafe {
		return Summary{}, graph.Fail(graph.Invalid,
			"unsupported rebase mode %q; only %q is accepted", batch.Rebase, change.RebaseSafe)
	}
	var next *graph.Graph
	if batch.Rebase == change.RebaseSafe && batch.Base != head.graph.Revision {
		base, err := db.rebaseBase(name, batch.Base)
		if err != nil {
			return Summary{}, err
		}
		next, err = change.Rebase(base.graph, head.graph, batch)
		if err != nil {
			return Summary{}, err
		}
	} else {
		next, err = change.Prepare(head.graph, batch)
		if err != nil {
			return Summary{}, err
		}
	}
	return db.publish(next)
}

// rebaseBase locates an immutable retained snapshot at the requested revision.
// Retention is bounded and ephemeral, so a missing snapshot is reported as an
// explicit rebase_unavailable rather than guessed at.
func (db *Database) rebaseBase(name string, revision uint64) (*state, error) {
	if revision == 0 || revision > db.heads[name].graph.Revision {
		return nil, graph.Fail(graph.RebaseUnavailable,
			"revision %d is outside the bounded rebase window", revision)
	}
	if revision == db.heads[name].graph.Revision {
		return db.heads[name], nil
	}
	for _, prior := range db.rear[name] {
		if prior.graph.Revision == revision {
			return prior, nil
		}
	}
	return nil, graph.Fail(graph.RebaseUnavailable,
		"revision %d is outside the bounded rebase window; refetch and reconcile manually", revision)
}

func (db *Database) mayInstall(name string) error {
	switch {
	case db.fault:
		return db.available()
	case !graph.ValidID(name):
		return graph.Fail(graph.Invalid, "invalid graph name")
	case db.heads[name] != nil:
		return graph.Fail(graph.Conflict, "graph %q already exists", name)
	case len(db.heads) == MaxGraphs:
		return graph.Fail(graph.Budget, "graph namespace limit reached")
	}
	return nil
}

func (db *Database) publish(g *graph.Graph) (Summary, error) {
	encoded, err := image.Encode(g)
	if err != nil {
		return Summary{}, err
	}
	next := materialize(g)
	uncertain, err := db.disk.Write(g.Name, encoded)
	if err != nil {
		log.Printf("publish graph %q revision %d: %v", g.Name, g.Revision, err)
		if uncertain {
			db.fault = true
			return Summary{}, db.available()
		}
		return Summary{}, graph.Fail(graph.Unavailable, "graph image could not be installed")
	}
	if previous := db.heads[g.Name]; previous != nil {
		db.rear[g.Name] = append([]*state{previous}, db.rear[g.Name]...)
		if len(db.rear[g.Name]) > MaxRebaseWindow {
			db.rear[g.Name] = db.rear[g.Name][:MaxRebaseWindow]
		}
	}
	db.heads[g.Name] = next
	return summary(g), nil
}

type RestoreResult struct {
	Installed Summary `json:"installed"`
	Source    Summary `json:"source"`
}

func (db *Database) Restore(name string, encoded image.Envelope) (RestoreResult, error) {
	g, err := checkedImage(encoded)
	if err != nil {
		return RestoreResult{}, err
	}
	source := summary(g)
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.mayInstall(name); err != nil {
		return RestoreResult{}, err
	}
	g.Name, g.Revision = name, 1
	installed, err := db.publish(g)
	if err != nil {
		return RestoreResult{}, err
	}
	// A restored image is a new local identity: stale snapshots from a prior
	// graph under this name must never anchor a rebase.
	db.rear[name] = nil
	return RestoreResult{installed, source}, nil
}
