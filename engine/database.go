package engine

import (
	"fmt"
	"sync"

	"rillweave/catalog"
	"rillweave/disk"
	"rillweave/graph"
	"rillweave/image"
	"rillweave/index"
	"rillweave/view"
)

const MaxGraphs = 32

type state struct {
	graph *graph.Graph
	adj   *index.Adjacency
	props *index.Postings
}

func materialize(g *graph.Graph) *state {
	return &state{g, index.BuildAdjacency(g), index.BuildPostings(g)}
}

type Database struct {
	mu    sync.RWMutex
	disk  *disk.Directory
	heads map[string]*state
	held  view.Handles[*state]
	fault bool
}

func Open(root string) (*Database, error) {
	dir, err := disk.Open(root)
	if err != nil {
		return nil, err
	}
	db := &Database{disk: dir, heads: make(map[string]*state), held: view.New[*state]()}
	err = dir.RestoreHeads(db.acceptHead)
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	return db, nil
}

func (db *Database) Close() error {
	return db.disk.Close()
}

func (db *Database) acceptHead(name string, encoded image.Envelope) error {
	if len(db.heads) == MaxGraphs {
		return fmt.Errorf("graph count exceeds %d", MaxGraphs)
	}
	g, err := checkedImage(encoded)
	if err != nil {
		return err
	}
	if name != g.Name {
		return fmt.Errorf("disk name %q does not match image name %q", name, g.Name)
	}
	db.heads[name] = materialize(g)
	return nil
}

func checkedImage(encoded image.Envelope) (*graph.Graph, error) {
	g, err := image.Decode(encoded)
	if err != nil {
		return nil, err
	}
	if err := catalog.Validate(g); err != nil {
		return nil, err
	}
	return g, nil
}

func (db *Database) available() error {
	if db.fault {
		return graph.Fail(graph.Unavailable, "durability is uncertain; restart to verify durable graph heads")
	}
	return nil
}

func (db *Database) Ready() error {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.available()
}
