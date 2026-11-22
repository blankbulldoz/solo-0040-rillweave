package engine

import (
	"errors"
	"fmt"
	"log"
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

// Quarantined describes one installed head file that quarantine recovery
// refused to serve. The file itself is left on disk untouched.
type Quarantined struct {
	Name    string `json:"name"`
	File    string `json:"file"`
	Claimed string `json:"claimed,omitempty"`
	Reason  string `json:"reason"`
}

// RecoveryReport is the operator-visible result of opening a data directory.
type RecoveryReport struct {
	Mode        string        `json:"mode"`
	Recovered   int           `json:"recovered"`
	Quarantined []Quarantined `json:"quarantined"`
}

type Database struct {
	mu          sync.RWMutex
	disk        *disk.Directory
	heads       map[string]*state
	held        view.Handles[*state]
	quarantined []Quarantined
	blocked     map[string]string // graph name -> why it may not be used
	isolating   bool
	fault       bool
}

type options struct {
	quarantine bool
}

// Option adjusts recovery behavior at Open.
type Option func(*options)

// IsolateUnusableHeads enables explicit quarantine recovery: unusable head
// files are reported and excluded, while every head that passes the complete
// image and catalog checks is served. Without this option the first unusable
// head file still aborts startup.
func IsolateUnusableHeads() Option {
	return func(o *options) { o.quarantine = true }
}

func Open(root string, opts ...Option) (*Database, error) {
	cfg := options{}
	for _, apply := range opts {
		apply(&cfg)
	}
	dir, err := disk.Open(root)
	if err != nil {
		return nil, err
	}
	db := &Database{
		disk: dir, heads: make(map[string]*state), held: view.New[*state](),
		blocked: make(map[string]string), isolating: cfg.quarantine,
	}
	if cfg.quarantine {
		err = db.recoverIsolating(dir)
	} else {
		err = dir.RestoreHeads(db.acceptHead)
	}
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
	g, err := db.checkHead(name, encoded)
	if err != nil {
		return err
	}
	db.heads[name] = materialize(g)
	return nil
}

// checkHead runs the catalog half of admission without changing the in-memory
// state. The graph-count limit depends on directory order, so recovery applies
// it when admitting, not here.
func (db *Database) checkHead(name string, encoded image.Envelope) (*graph.Graph, error) {
	g, err := checkedImage(encoded)
	if err != nil {
		return nil, err
	}
	if name != g.Name {
		return nil, &nameMismatch{disk: name, image: g.Name}
	}
	return g, nil
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

// recoverIsolating inspects every installed head twice: first to decide which
// graphs are fully valid, then to record the rest as quarantined. A name
// claimed by a rejected file never hides a graph that its own matching file
// proved valid.
func (db *Database) recoverIsolating(dir *disk.Directory) error {
	type candidate struct {
		entry disk.HeadEntry
		graph *graph.Graph
		err   error
	}
	var candidates []candidate
	err := dir.ScanHeads(func(entry disk.HeadEntry) error {
		c := candidate{entry: entry, err: entry.Err}
		// A structural entry (for example a directory named like a head) cannot
		// pass this seam, so it is isolated as-is.
		if c.err == nil {
			c.graph, c.err = db.checkHead(entry.Name, entry.Encoded)
		}
		candidates = append(candidates, c)
		return nil
	})
	if err != nil {
		return err
	}
	valid := make(map[string]bool)
	for _, c := range candidates {
		if c.err == nil {
			valid[c.entry.Name] = true
		}
	}
	for _, c := range candidates {
		if c.err == nil && len(db.heads) < MaxGraphs {
			db.heads[c.entry.Name] = materialize(c.graph)
			continue
		}
		if c.err == nil {
			c.err = fmt.Errorf("graph count exceeds %d", MaxGraphs)
		}
		db.quarantine(c.entry, c.err, valid)
	}
	return nil
}

func (db *Database) quarantine(entry disk.HeadEntry, cause error, valid map[string]bool) {
	name := entry.Name
	file := entry.File
	if file == "" {
		file = name + ".rill"
	}
	record := Quarantined{Name: name, File: file, Reason: cause.Error()}
	if graph.ValidID(name) {
		db.blocked[name] = fmt.Sprintf("head file %q: %s", file, cause)
	}
	// A mismatched file still names a graph inside its payload. Report that
	// name, and reserve it only when no valid head file proves it.
	if claimed := claimedName(cause); claimed != "" && claimed != name {
		record.Claimed = claimed
		if graph.ValidID(claimed) && !valid[claimed] {
			db.blocked[claimed] = fmt.Sprintf("name is claimed by head file %q", file)
		}
	}
	db.quarantined = append(db.quarantined, record)
	log.Printf("quarantine graph %q (%s): %v", name, file, cause)
}

func claimedName(cause error) string {
	var mismatch *nameMismatch
	if errors.As(cause, &mismatch) {
		return mismatch.image
	}
	return ""
}

type nameMismatch struct {
	disk, image string
}

func (m *nameMismatch) Error() string {
	return fmt.Sprintf("disk name %q does not match image name %q", m.disk, m.image)
}

func (db *Database) Report() RecoveryReport {
	db.mu.RLock()
	defer db.mu.RUnlock()
	report := RecoveryReport{
		Mode: "strict", Recovered: len(db.heads),
		Quarantined: append([]Quarantined{}, db.quarantined...),
	}
	if db.isolating {
		report.Mode = "quarantine"
	}
	return report
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
