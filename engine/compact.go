package engine

import (
	"log"

	"rillweave/compress"
	"rillweave/graph"
	"rillweave/image"
)

// CompactResult is returned when a fixed revision is compressed. The envelope
// is the portable, digest-protected image; Stats quantifies the reduction.
type CompactResult struct {
	Envelope     compress.Envelope `json:"envelope"`
	Source       Summary           `json:"source"`
	Stats        compress.Stats    `json:"stats"`
	SourceBytes  int               `json:"source_bytes"`
	PayloadBytes int               `json:"payload_bytes"`
	ByteRatio    float64           `json:"byte_ratio"`
}

// Compress builds a compressed document from a pinned or head revision without
// installing it, so the caller can archive or transport the image.
func (db *Database) Compress(name, handle string, req compress.Request) (CompactResult, error) {
	s, err := db.read(name, handle)
	if err != nil {
		return CompactResult{}, err
	}
	doc, err := compress.Build(s.graph, s.adj, req)
	if err != nil {
		return CompactResult{}, err
	}
	envelope, err := compress.Encode(doc)
	if err != nil {
		return CompactResult{}, err
	}
	source, err := image.Encode(s.graph)
	if err != nil {
		return CompactResult{}, err
	}
	result := CompactResult{
		Envelope: envelope, Source: summary(s.graph),
		Stats: doc.Stats, SourceBytes: len(source.Payload),
		PayloadBytes: len(envelope.Payload),
	}
	result.ByteRatio = float64(result.PayloadBytes) / float64(result.SourceBytes)
	return result, nil
}

// InstallResult records a compressed graph installation.
type InstallResult struct {
	Installed string         `json:"installed"`
	Source    Summary        `json:"source"`
	Stats     compress.Stats `json:"stats"`
}

// InstallCompressed validates and durably installs a compressed image under an
// unused namespace. Restart reopens it without the source instance.
func (db *Database) InstallCompressed(encoded compress.Envelope) (InstallResult, error) {
	doc, err := compress.Decode(encoded)
	if err != nil {
		return InstallResult{}, err
	}
	network, err := compress.Materialize(doc)
	if err != nil {
		return InstallResult{}, err
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.mayInstallCompact(doc.Name); err != nil {
		return InstallResult{}, err
	}
	uncertain, err := db.disk.WriteCompact(doc.Name, encoded)
	if err != nil {
		log.Printf("install compressed graph %q: %v", doc.Name, err)
		if uncertain {
			db.fault = true
			return InstallResult{}, db.available()
		}
		return InstallResult{}, graph.Fail(graph.Unavailable, "compressed image could not be installed")
	}
	db.compacts[doc.Name] = &compactState{doc, network}
	return InstallResult{
		Installed: doc.Name,
		Source:    Summary{Name: doc.SourceName, Revision: doc.SourceRevision},
		Stats:     doc.Stats,
	}, nil
}

func (db *Database) mayInstallCompact(name string) error {
	switch {
	case db.fault:
		return db.available()
	case !graph.ValidID(name):
		return graph.Fail(graph.Invalid, "invalid compressed graph name")
	case db.heads[name] != nil:
		return graph.Fail(graph.Conflict, "graph %q already exists as a regular graph", name)
	case db.compacts[name] != nil:
		return graph.Fail(graph.Conflict, "compressed graph %q already exists", name)
	case len(db.compacts) == compress.MaxInstallations:
		return graph.Fail(graph.Budget, "compressed graph namespace limit reached")
	}
	return nil
}

func (db *Database) readCompact(name string) (*compactState, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if err := db.available(); err != nil {
		return nil, err
	}
	if state, ok := db.compacts[name]; ok {
		return state, nil
	}
	return nil, graph.Fail(graph.Missing, "compressed graph %q not found", name)
}

// CompactRead wraps one compressed-graph read in the same envelope shape as
// regular graph reads, so clients always receive graph/revision/result.
type CompactRead[T any] struct {
	Graph    string `json:"graph"`
	Revision uint64 `json:"source_revision"`
	Result   T      `json:"result"`
}

func compactEnvelope[T any](state *compactState, value T) CompactRead[T] {
	return CompactRead[T]{
		Graph: state.doc.Name, Revision: state.doc.SourceRevision, Result: value,
	}
}

// CompactOverview describes an installed compressed graph without returning
// every retained record.
type CompactOverview struct {
	Name         string         `json:"name"`
	Source       Summary        `json:"source"`
	Kinds        []string       `json:"kinds"`
	Endpoints    []string       `json:"endpoints"`
	Clusters     int            `json:"clusters"`
	Stats        compress.Stats `json:"stats"`
	PayloadBytes int            `json:"payload_bytes"`
	Digest       string         `json:"sha256"`
}

// CompactOverview returns the manifest-level description and statistics.
func (db *Database) CompactOverview(name string) (CompactRead[CompactOverview], error) {
	state, err := db.readCompact(name)
	if err != nil {
		return CompactRead[CompactOverview]{}, err
	}
	doc := state.doc
	envelope, err := compress.Encode(doc)
	if err != nil {
		return CompactRead[CompactOverview]{}, err
	}
	overview := CompactOverview{
		Name:      doc.Name,
		Source:    Summary{Name: doc.SourceName, Revision: doc.SourceRevision},
		Kinds:     append([]string{}, doc.Kinds...),
		Endpoints: append([]string{}, doc.Endpoints...),
		Clusters:  len(doc.Clusters), Stats: doc.Stats,
		PayloadBytes: len(envelope.Payload), Digest: envelope.Digest,
	}
	return compactEnvelope(state, overview), nil
}

// CompactDocument returns the full portable document for offline archiving.
func (db *Database) CompactDocument(name string) (compress.Envelope, error) {
	state, err := db.readCompact(name)
	if err != nil {
		return compress.Envelope{}, err
	}
	return compress.Encode(state.doc)
}

// CompactRoute answers one minimum-cost query on the installed summary.
func (db *Database) CompactRoute(name string, req compress.RouteRequest) (CompactRead[compress.RouteAnswer], error) {
	state, err := db.readCompact(name)
	if err != nil {
		return CompactRead[compress.RouteAnswer]{}, err
	}
	answer, err := state.net.Route(req)
	if err != nil {
		return CompactRead[compress.RouteAnswer]{}, err
	}
	return compactEnvelope(state, answer), nil
}

// CompactReplay verifies a witness against retained arc records only.
func (db *Database) CompactReplay(name string, arcIDs []string) (CompactRead[compress.Verification], error) {
	state, err := db.readCompact(name)
	if err != nil {
		return CompactRead[compress.Verification]{}, err
	}
	return compactEnvelope(state, state.net.Replay(arcIDs)), nil
}
