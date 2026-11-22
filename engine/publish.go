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
	next, err := admit(head, batch)
	if err != nil {
		return Summary{}, err
	}
	return db.publish(next)
}

// admit is the single admission path for a batch. Commit and Rehearse both use
// it, so a candidate that a rehearsal accepts is exactly one that a commit on
// the same baseline would accept.
func admit(base *state, batch change.Batch) (*graph.Graph, error) {
	return change.Prepare(base.graph, batch)
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
	return RestoreResult{installed, source}, nil
}
