// Package compress turns one fixed graph revision, restricted to a set of
// selected arc kinds, into a durable summary graph built around its strongly
// connected components (the "ring clusters" exposed by the components
// workflow).
//
// The summary keeps every cross-group arc with its original identity and only
// the boundary-to-boundary shortest routes inside each cluster. A route on the
// summary is therefore a faithful, replayable walk of the original graph: each
// summary edge expands either into one original cross-group arc or into a
// stored original-arc witness. Internal vertices and unreferenced internal
// arcs never leave the source instance.
package compress

const (
	// Format is the version of the compressed document envelope.
	Format = 1
	// MaxEndpoints bounds the exact-tier endpoint set supplied at export.
	MaxEndpoints = 1000
	// MaxBuildBudget bounds candidate arc inspections while compressing.
	MaxBuildBudget = 6000000
	// DefaultBuildBudget mirrors the read-side default when none is supplied.
	DefaultBuildBudget = 300000
	// MaxPayload bounds the portable compressed payload (see image limits).
	MaxPayload = 12 << 20
	// MaxDocument bounds the whole envelope accepted over the wire.
	MaxDocument = (MaxPayload*4)/3 + 1024
	// MaxInstallations bounds compressed graphs held by one process.
	MaxInstallations = 32
)

// ArcRecord keeps the original identity of an arc retained by the summary:
// ID, kind, endpoints and cost. Cross-group arcs and arcs referenced by an
// intra-cluster witness are kept; every other original arc, including its
// full property payload, stays in the source instance only.
type ArcRecord struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	From string `json:"from"`
	To   string `json:"to"`
	Cost int64  `json:"cost"`
}

// Cluster describes one strongly connected component of the selected subgraph.
type Cluster struct {
	ID          string   `json:"id"`
	Boundaries  []string `json:"boundaries"`
	MemberCount int      `json:"member_count"`
}

// CrossArc keeps an original arc whose endpoints fall in different clusters.
// Parallel cross arcs (different kinds or costs) stay individually addressable.
type CrossArc struct {
	ArcID     string `json:"arc_id"`
	Kind      string `json:"kind"`
	From      string `json:"from"`
	To        string `json:"to"`
	FromGroup string `json:"from_group"`
	ToGroup   string `json:"to_group"`
	Cost      int64  `json:"cost"`
}

// Witness is one deduplicated original-arc sequence stored in the shared
// pool. Summary edges reference witnesses by ID, so ring prefixes shared by
// many boundary pairs are recorded once rather than copied per edge.
type Witness struct {
	ID     string   `json:"id"`
	ArcIDs []string `json:"arc_ids"`
	Cost   int64    `json:"cost"`
	Hops   int      `json:"hops"`
}

// SummaryEdge is a cheapest intra-cluster route between two boundary vertices.
// Several edges may point at the same witness when they share a full sequence,
// and different witnesses can still share long arc-ID prefixes.
type SummaryEdge struct {
	ID        string `json:"id"`
	Cluster   string `json:"cluster"`
	From      string `json:"from"`
	To        string `json:"to"`
	WitnessID string `json:"witness_id"`
	Cost      int64  `json:"cost"`
	Hops      int    `json:"hops"`
	Shared    bool   `json:"shared,omitempty"`
}

// Stats quantify the compression against the selected subgraph. Byte-level
// ratios are added by the engine layer, which knows the source image size.
type Stats struct {
	OriginalVertices int     `json:"original_vertices"`
	OriginalArcs     int     `json:"original_arcs"`
	RetainedVertices int     `json:"retained_vertices"`
	RetainedArcs     int     `json:"retained_arcs"`
	Clusters         int     `json:"clusters"`
	TrivialClusters  int     `json:"trivial_clusters"`
	CrossArcs        int     `json:"cross_arcs"`
	SummaryEdges     int     `json:"summary_edges"`
	Witnesses        int     `json:"witnesses"`
	SharedWitnesses  int     `json:"shared_witnesses"`
	ArcRatio         float64 `json:"arc_ratio"`
}

// Document is the installable, restartable compressed graph.
type Document struct {
	// Name is the compressed graph's own namespace; SourceName/SourceRevision
	// identify the fixed revision it was derived from.
	Name           string   `json:"name"`
	SourceName     string   `json:"source_name"`
	SourceRevision uint64   `json:"source_revision"`
	Kinds          []string `json:"kinds"`
	// Endpoints is the exact-tier set agreed at export.
	Endpoints []string      `json:"endpoints"`
	Clusters  []Cluster     `json:"clusters"`
	CrossArcs []CrossArc    `json:"cross_arcs"`
	Summaries []SummaryEdge `json:"summaries"`
	// Witnesses is the shared, deduplicated pool of original-arc sequences.
	Witnesses []Witness   `json:"witnesses"`
	ArcDict   []ArcRecord `json:"arc_dict"`
	Stats     Stats       `json:"stats"`
}

// Envelope wraps a canonical JSON payload with its SHA-256 digest, mirroring
// the integrity envelope used by full graph images.
type Envelope struct {
	Format  int    `json:"format"`
	Digest  string `json:"sha256"`
	Payload []byte `json:"payload"`
}
