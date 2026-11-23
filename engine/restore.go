package engine

import (
	"encoding/json"
	"log"
	"math"
	"unicode/utf8"

	"rillweave/catalog"
	"rillweave/graph"
	"rillweave/image"
)

// RestoreRequest installs a source image. An unused target name installs a new
// graph at revision one, as before. An occupied target name requires an
// explicit expected head revision and replaces the complete graph at the next
// local revision, so an integer write precondition that named the old head can
// never become valid again through the replacement.
type RestoreRequest struct {
	Name      string
	Envelope  image.Envelope
	Operation string
	Expected  *uint64
}

type RestoreResult struct {
	Operation string  `json:"operation,omitempty"`
	Expected  *uint64 `json:"expected,omitempty"`
	Installed Summary `json:"installed"`
	Source    Summary `json:"source"`
	Created   bool    `json:"-"`
}

// restoreReceipt is the durable record of the most recent successful restore
// action that carried an operation identifier. It names the source image and
// the local revision installed by that action, so an identical retry can
// replay it without publishing. An absent Expect records a fresh install.
type restoreReceipt struct {
	Operation       string  `json:"operation"`
	Expect          *uint64 `json:"expect,omitempty"`
	Format          int     `json:"format"`
	Digest          string  `json:"sha256"`
	InstalledDigest string  `json:"installed_sha256"`
	Installed       Summary `json:"installed"`
	Source          Summary `json:"source"`
}

// pendingRestore records an overwrite that may or may not have reached the
// durable head. Both the receipt it intends to retain and the receipt it
// replaces are kept, so recovery can commit or roll it back without guessing.
type pendingRestore struct {
	New      *restoreReceipt `json:"new,omitempty"`
	Previous *restoreReceipt `json:"previous,omitempty"`
}

func (r *restoreReceipt) result() RestoreResult {
	return RestoreResult{
		Operation: r.Operation, Expected: r.Expect,
		Installed: r.Installed, Source: r.Source,
	}
}

func validOperation(op string) bool {
	return op != "" && len(op) <= 128 && utf8.ValidString(op)
}

func sameExpect(a, b *uint64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// replay resolves an operation identifier that is still retained for the
// target graph. A byte-identical image under the same expected head replays
// the recorded receipt without touching the head; any other reuse conflicts
// for as long as the receipt is retained.
func (db *Database) replay(req RestoreRequest) (RestoreResult, bool, error) {
	if req.Operation == "" {
		return RestoreResult{}, false, nil
	}
	receipt := db.receipts[req.Name]
	if receipt == nil || receipt.Operation != req.Operation {
		return RestoreResult{}, false, nil
	}
	if !sameExpect(req.Expected, receipt.Expect) ||
		req.Envelope.Format != receipt.Format || req.Envelope.Digest != receipt.Digest {
		return RestoreResult{}, true, graph.Fail(graph.Conflict,
			"operation %q was already used on graph %q with a different image or expected head",
			req.Operation, req.Name)
	}
	// Replayed bytes must still be the installed bytes, so a damaged
	// retransmission is invalid rather than silently accepted.
	if err := image.Verify(req.Envelope); err != nil {
		return RestoreResult{}, true, err
	}
	return receipt.result(), true, nil
}

func (db *Database) Restore(req RestoreRequest) (RestoreResult, error) {
	if req.Operation != "" && !validOperation(req.Operation) {
		return RestoreResult{}, graph.Fail(graph.Invalid,
			"operation must be nonempty UTF-8 of at most 128 bytes")
	}

	// A retained operation replays before image decoding so a lost response
	// can be reconciled even when the replacement is not resent byte for byte.
	// Conflicting reuse stays a conflict either way.
	db.mu.Lock()
	if err := db.available(); err != nil {
		db.mu.Unlock()
		return RestoreResult{}, err
	}
	if replayed, found, err := db.replay(req); found {
		db.mu.Unlock()
		return replayed, err
	}
	db.mu.Unlock()

	g, err := checkedImage(req.Envelope)
	if err != nil {
		return RestoreResult{}, err
	}
	source := summary(g)

	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.available(); err != nil {
		return RestoreResult{}, err
	}
	if replayed, found, err := db.replay(req); found {
		return replayed, err
	}
	if head := db.heads[req.Name]; head != nil {
		return db.replace(req, head, g, source)
	}
	return db.installFresh(req, g, source)
}

// installFresh keeps the original empty-name import: the target must be free
// and starts at revision one. An operation identifier makes the import
// retryable after a lost response, just like an overwrite.
func (db *Database) installFresh(req RestoreRequest, g *graph.Graph, source Summary) (RestoreResult, error) {
	if req.Expected != nil {
		return RestoreResult{}, graph.Fail(graph.Conflict,
			"graph %q does not exist; expected head %d cannot be satisfied", req.Name, *req.Expected)
	}
	if err := db.mayInstall(req.Name); err != nil {
		return RestoreResult{}, err
	}
	g.Name, g.Revision = req.Name, 1
	next := &restoreReceipt{
		Operation: req.Operation,
		Format:    req.Envelope.Format,
		Digest:    req.Envelope.Digest,
		Installed: summary(g),
		Source:    source,
	}
	encoded, err := image.Encode(g)
	if err != nil {
		return RestoreResult{}, err
	}
	next.InstalledDigest = encoded.Digest
	previous := db.receipts[req.Name]
	pending := pendingRestore{New: next, Previous: previous}
	if err := db.writePending(req.Name, pending); err != nil {
		return RestoreResult{}, err
	}
	if req.Operation != "" {
		if err := db.storeReceipt(req.Name, next); err != nil {
			db.finishPending(req.Name, pending.Previous)
			return RestoreResult{}, err
		}
	}
	if _, err := db.installEncoded(g, encoded); err != nil {
		if !db.fault {
			db.finishPending(req.Name, pending.Previous)
			return RestoreResult{}, err
		}
		return RestoreResult{}, err
	}
	if req.Operation == "" {
		db.dropReceipt(req.Name)
	}
	db.clearPending(req.Name)
	result := next.result()
	result.Created = true
	return result, nil
}

func (db *Database) replace(req RestoreRequest, head *state, g *graph.Graph, source Summary) (RestoreResult, error) {
	if req.Expected == nil {
		return RestoreResult{}, graph.Fail(graph.Conflict,
			"graph %q already exists; an overwrite requires the expected head revision", req.Name)
	}
	current := head.graph.Revision
	if *req.Expected != current {
		return RestoreResult{}, graph.Fail(graph.Conflict,
			"expected head %d is not current revision %d", *req.Expected, current)
	}
	if current == math.MaxUint64 {
		return RestoreResult{}, graph.Fail(graph.Budget, "revision space exhausted")
	}
	expected := current

	g.Name, g.Revision = req.Name, current+1
	if err := catalog.Validate(g); err != nil {
		return RestoreResult{}, err
	}
	encoded, err := image.Encode(g)
	if err != nil {
		return RestoreResult{}, err
	}
	next := &restoreReceipt{
		Operation:       req.Operation,
		Expect:          &expected,
		Format:          encoded.Format,
		Digest:          req.Envelope.Digest,
		InstalledDigest: encoded.Digest,
		Installed:       summary(g),
		Source:          source,
	}
	previous := db.receipts[req.Name]

	// Record the intent before touching either durable file. The pending
	// record names both receipts, so a crash between writing the new receipt
	// and publishing the head can be rolled back on restart.
	pending := pendingRestore{New: next, Previous: previous}
	if err := db.writePending(req.Name, pending); err != nil {
		return RestoreResult{}, err
	}
	if req.Operation != "" {
		if err := db.storeReceipt(req.Name, next); err != nil {
			db.finishPending(req.Name, pending.Previous)
			return RestoreResult{}, err
		}
	}
	if _, err := db.installEncoded(g, encoded); err != nil {
		// Without confirmed durability the head may or may not have moved, so
		// the pending record is left for restart reconciliation.
		if !db.fault {
			db.finishPending(req.Name, pending.Previous)
			return RestoreResult{}, err
		}
		return RestoreResult{}, err
	}
	// Only the most recent successful overwrite keeps a receipt. A newer
	// overwrite without an identifier clears it, so older retries fail
	// explicitly instead of replaying against a replaced head.
	if req.Operation == "" {
		db.dropReceipt(req.Name)
	} else {
		db.receipts[req.Name] = next
	}
	db.clearPending(req.Name)
	return next.result(), nil
}

func (db *Database) writePending(name string, pending pendingRestore) error {
	raw, err := json.Marshal(pending)
	if err != nil {
		return err
	}
	uncertain, err := db.disk.WritePending(name, raw)
	if err != nil {
		log.Printf("record restore intent for %q: %v", name, err)
		if uncertain {
			db.fault = true
			return db.available()
		}
		return graph.Fail(graph.Unavailable, "restore intent could not be recorded")
	}
	return nil
}

// storeReceipt persists a receipt and records it in memory.
func (db *Database) storeReceipt(name string, receipt *restoreReceipt) error {
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if err := db.persistReceipt(name, raw); err != nil {
		return err
	}
	db.receipts[name] = receipt
	return nil
}

func (db *Database) persistReceipt(name string, raw []byte) error {
	uncertain, err := db.disk.WriteReceipt(name, raw)
	if err != nil {
		log.Printf("retain restore receipt for %q: %v", name, err)
		if uncertain {
			db.fault = true
			return db.available()
		}
		return graph.Fail(graph.Unavailable, "restore receipt could not be installed")
	}
	return nil
}

// rewriteReceipt puts a receipt back on disk during recovery, which is what an
// interrupted overwrite needs to restore its predecessor.
func (db *Database) rewriteReceipt(name string, receipt *restoreReceipt) error {
	if receipt == nil || receipt.Operation == "" {
		return db.disk.RemoveReceipt(name)
	}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	return db.persistReceipt(name, raw)
}

// finishPending rolls an interrupted overwrite back to the receipt that was
// retained before it started.
func (db *Database) finishPending(name string, previous *restoreReceipt) {
	if err := db.rewriteReceipt(name, previous); err != nil {
		log.Printf("restore previous receipt for %q: %v", name, err)
	} else if previous != nil && previous.Operation != "" {
		db.receipts[name] = previous
	} else {
		delete(db.receipts, name)
	}
	db.clearPending(name)
}

func (db *Database) clearPending(name string) {
	if err := db.disk.RemovePending(name); err != nil {
		log.Printf("clear restore intent for %q: %v", name, err)
	}
}

func (db *Database) dropReceipt(name string) {
	delete(db.receipts, name)
	if err := db.disk.RemoveReceipt(name); err != nil {
		log.Printf("remove restore receipt for %q: %v", name, err)
	}
}
