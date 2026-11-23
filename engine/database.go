package engine

import (
	"bytes"
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
	graph  *graph.Graph
	digest string
	adj    *index.Adjacency
	props  *index.Postings
}

func materialize(g *graph.Graph, digest string) *state {
	return &state{g, digest, index.BuildAdjacency(g), index.BuildPostings(g)}
}

type Database struct {
	mu       sync.RWMutex
	disk     *disk.Directory
	heads    map[string]*state
	receipts map[string]*restoreReceipt
	held     view.Handles[*state]
	fault    bool
}

func Open(root string) (*Database, error) {
	dir, err := disk.Open(root)
	if err != nil {
		return nil, err
	}
	db := &Database{
		disk: dir, heads: make(map[string]*state), receipts: make(map[string]*restoreReceipt),
		held: view.New[*state](),
	}
	err = dir.RestoreHeads(db.acceptHead)
	if err == nil {
		err = db.reconcileRestores()
	}
	if err != nil {
		_ = dir.Close()
		return nil, err
	}
	return db, nil
}

// reconcileRestores resolves overwrite intents left by a crash and then loads
// the retained receipts. An intent whose head publication reached the durable
// head confirms the new receipt; one whose head is unchanged rolls back to the
// receipt that was retained before the overwrite started.
//
// Receipts are auxiliary: they make retries idempotent but do not describe the
// installed graph, so a malformed entry is discarded rather than blocking
// startup. Read and synchronization faults still fail startup.
func (db *Database) reconcileRestores() error {
	pendings, err := db.disk.ReadPendings()
	if err != nil {
		return err
	}
	for name, payload := range pendings {
		var pending pendingRestore
		if err := image.ReadJSON(bytes.NewReader(payload), &pending); err != nil {
			log.Printf("discard malformed restore intent for %q: %v", name, err)
			if err := db.disk.RemovePending(name); err != nil {
				return err
			}
			continue
		}
		head := db.heads[name]
		installed := pending.New != nil && head != nil &&
			head.graph.Revision == pending.New.Installed.Revision &&
			head.digest == pending.New.InstalledDigest
		switch {
		case installed:
			// The head publication completed; confirm its receipt. A restore
			// without an identifier intentionally retains none.
			if pending.New.Operation != "" {
				if err := db.rewriteReceipt(name, pending.New); err != nil {
					return err
				}
			} else if err := db.disk.RemoveReceipt(name); err != nil {
				return err
			}
		case head == nil:
			// The graph was never installed, so no receipt can describe it.
			if err := db.disk.RemoveReceipt(name); err != nil {
				return err
			}
		default:
			// The head never moved; the previous receipt is still current.
			if err := db.rewriteReceipt(name, pending.Previous); err != nil {
				return err
			}
		}
		if err := db.disk.RemovePending(name); err != nil {
			return err
		}
	}
	return db.acceptReceipts()
}

// acceptReceipts loads the retained overwrite receipts. A receipt survives
// only when the current head can be explained by it: either the head is
// exactly the revision it installed, or ordinary commits advanced the same
// graph above that revision. Anything else cannot be replayed safely and is
// dropped.
func (db *Database) acceptReceipts() error {
	raw, err := db.disk.ReadReceipts()
	if err != nil {
		return err
	}
	for name, payload := range raw {
		head := db.heads[name]
		if head == nil {
			if err := db.disk.RemoveReceipt(name); err != nil {
				return err
			}
			continue
		}
		var receipt restoreReceipt
		if err := image.ReadJSON(bytes.NewReader(payload), &receipt); err != nil {
			log.Printf("discard malformed restore receipt for %q: %v", name, err)
			if err := db.disk.RemoveReceipt(name); err != nil {
				return err
			}
			continue
		}
		if !receipt.describes(name, head) {
			if err := db.disk.RemoveReceipt(name); err != nil {
				return err
			}
			continue
		}
		db.receipts[name] = &receipt
	}
	return nil
}

// describes reports whether the retained receipt is consistent with the
// installed graph. A fresh install has no expected head and must have landed
// at revision 1; an overwrite must have landed exactly one past its expected
// head, and the current head may not be below either result. When the head is
// still that installed revision, the retained head digest must match the
// receipt, so an unrelated replacement that reached the same revision cannot
// inherit it.
func (r *restoreReceipt) describes(name string, head *state) bool {
	g := head.graph
	from := uint64(0)
	if r.Expect != nil {
		from = *r.Expect
	}
	if r.Operation == "" || !validOperation(r.Operation) ||
		r.Installed.Name != name || r.Installed.Revision != from+1 ||
		r.Installed.Revision > g.Revision || r.Format != image.Format ||
		r.Digest == "" || r.InstalledDigest == "" || r.Source.Revision == 0 {
		return false
	}
	if r.Installed.Revision != g.Revision {
		return true
	}
	return head.digest == r.InstalledDigest
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
	db.heads[name] = materialize(g, encoded.Digest)
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
