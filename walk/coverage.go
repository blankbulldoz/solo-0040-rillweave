package walk

import (
	"slices"

	"rillweave/graph"
	"rillweave/index"
)

// CoverageRequest groups minimum-hop reach from a root by a typed vertex
// property. Values restricts the value buckets to the supplied low-frequency
// values; vertices holding any other value fall into the explicit "other"
// bucket so counts stay exact.
type CoverageRequest struct {
	Root     string         `json:"root"`
	MaxHops  int            `json:"max_hops"`
	Kind     string         `json:"kind"`
	Property string         `json:"property"`
	Values   []graph.Scalar `json:"values,omitempty"`
	Limit    int            `json:"limit,omitempty"`
	Selection
}

// Bucket counts first arrivals sharing one property outcome on one layer.
// Class is "value", "other", "missing" or "undeclared". Value buckets carry
// the typed scalar; the other classes do not. Count is always exact; the
// witness window is bounded by the request limit.
type Bucket struct {
	Class     string        `json:"class"`
	Value     *graph.Scalar `json:"value,omitempty"`
	Count     int           `json:"count"`
	Witnesses []string      `json:"witnesses"`
	Truncated bool          `json:"truncated,omitempty"`
}

// Layer holds the buckets contributed at one exact minimum hop distance.
type Layer struct {
	Hops    int      `json:"hops"`
	Reached int      `json:"reached"`
	Buckets []Bucket `json:"buckets"`
}

// CoverageResult is the full same-view census. Totals aggregate every layer;
// a result is emitted only when the bounded traversal completed.
type CoverageResult struct {
	Root     string   `json:"root"`
	Layers   []Layer  `json:"layers"`
	Totals   []Bucket `json:"totals"`
	Scanned  int      `json:"scanned"`
	Complete bool     `json:"complete"`
}

const (
	bucketValue      = "value"
	bucketOther      = "other"
	bucketMissing    = "missing"
	bucketUndeclared = "undeclared"
)

type bucketKey struct {
	class string
	value graph.Scalar
}

type bucketAcc struct {
	key       bucketKey
	count     int
	witnesses []string
	truncated bool
}

type coverageRun struct {
	g        *graph.Graph
	kind     string
	property string
	values   map[graph.Scalar]bool
	limit    int
	perLayer map[int]map[bucketKey]*bucketAcc
	totals   map[bucketKey]*bucketAcc
}

// Coverage runs a breadth-first minimum-hop expansion and buckets each first
// arrival by the requested typed property. Budget exhaustion rejects the whole
// query; a partial census is never presented as a complete one.
func Coverage(g *graph.Graph, a *index.Adjacency, q CoverageRequest) (CoverageResult, error) {
	if err := hops(q.MaxHops); err != nil {
		return CoverageResult{}, err
	}
	t, err := topologyFor(g, a, q.Selection)
	if err != nil {
		return CoverageResult{}, err
	}
	if err := requireVertex(g, q.Root); err != nil {
		return CoverageResult{}, err
	}
	limit, err := Limit(q.Limit)
	if err != nil {
		return CoverageResult{}, err
	}
	run := &coverageRun{
		g: g, kind: q.Kind, property: q.Property, limit: limit,
		values:   make(map[graph.Scalar]bool, len(q.Values)),
		perLayer: map[int]map[bucketKey]*bucketAcc{},
		totals:   map[bucketKey]*bucketAcc{},
	}
	for _, value := range q.Values {
		run.values[value] = true
	}

	seen := map[string]bool{q.Root: true}
	frontier := []string{q.Root}
	run.arrive(0, q.Root)
	for depth := 0; depth < q.MaxHops && len(frontier) != 0; depth++ {
		var next []string
		for _, id := range frontier {
			err := t.visit(id, t.direction, func(_ graph.Arc, target string) error {
				if !seen[target] {
					seen[target] = true
					next = append(next, target)
					run.arrive(depth+1, target)
				}
				return nil
			})
			if err != nil {
				return CoverageResult{}, err
			}
		}
		frontier = next
	}

	layers := make([]Layer, 0, q.MaxHops+1)
	for depth := 0; depth <= q.MaxHops; depth++ {
		buckets := run.emit(run.perLayer[depth])
		reached := 0
		for _, bucket := range buckets {
			reached += bucket.Count
		}
		layers = append(layers, Layer{Hops: depth, Reached: reached, Buckets: buckets})
	}
	return CoverageResult{
		Root: q.Root, Layers: layers, Totals: run.emit(run.totals),
		Scanned: t.scanned, Complete: true,
	}, nil
}

func (r *coverageRun) arrive(depth int, id string) {
	vertex := r.g.Vertices[id]
	// Classify against the vertex's own kind rule. A reached vertex of another
	// kind whose catalog never declares the property is "undeclared", which is
	// distinct from a declared property left absent on the record.
	_, declared := r.g.Schema.Vertices[vertex.Kind].Properties[r.property]
	value, present := vertex.Properties[r.property]
	var key bucketKey
	switch {
	case !declared:
		key = bucketKey{class: bucketUndeclared}
	case !present:
		key = bucketKey{class: bucketMissing}
	case len(r.values) != 0 && !r.values[value]:
		key = bucketKey{class: bucketOther}
	default:
		key = bucketKey{class: bucketValue, value: value}
	}
	r.recordLayer(depth, key, id)
	r.recordTotal(key, id)
}

func (r *coverageRun) recordLayer(depth int, key bucketKey, id string) {
	buckets, ok := r.perLayer[depth]
	if !ok {
		buckets = map[bucketKey]*bucketAcc{}
		r.perLayer[depth] = buckets
	}
	r.observe(buckets, key, id)
}

func (r *coverageRun) recordTotal(key bucketKey, id string) {
	r.observe(r.totals, key, id)
}

func (r *coverageRun) observe(buckets map[bucketKey]*bucketAcc, key bucketKey, id string) {
	acc, ok := buckets[key]
	if !ok {
		acc = &bucketAcc{key: key}
		buckets[key] = acc
	}
	acc.count++
	if len(acc.witnesses) < r.limit {
		acc.witnesses = append(acc.witnesses, id)
	} else {
		acc.truncated = true
	}
}

func (r *coverageRun) emit(table map[bucketKey]*bucketAcc) []Bucket {
	accs := make([]*bucketAcc, 0, len(table))
	for _, acc := range table {
		accs = append(accs, acc)
	}
	slices.SortFunc(accs, func(a, b *bucketAcc) int { return compareBucket(a.key, b.key) })
	out := make([]Bucket, 0, len(accs))
	for _, acc := range accs {
		bucket := Bucket{
			Class: acc.key.class, Count: acc.count,
			Witnesses: acc.witnesses, Truncated: acc.truncated,
		}
		if acc.key.class == bucketValue {
			value := acc.key.value
			bucket.Value = &value
		}
		out = append(out, bucket)
	}
	return out
}

func compareBucket(a, b bucketKey) int {
	if rank(a) != rank(b) {
		return rank(a) - rank(b)
	}
	if a.class == bucketValue {
		if n := compareScalar(a.value, b.value); n != 0 {
			return n
		}
	}
	return 0
}

func rank(key bucketKey) int {
	switch key.class {
	case bucketValue:
		return 0
	case bucketOther:
		return 1
	case bucketMissing:
		return 2
	default:
		return 3
	}
}

func compareScalar(a, b graph.Scalar) int {
	switch a.Kind {
	case graph.Text:
		if a.Text != b.Text {
			if a.Text < b.Text {
				return -1
			}
			return 1
		}
	case graph.Int:
		if a.Int != b.Int {
			if a.Int < b.Int {
				return -1
			}
			return 1
		}
	case graph.Bool:
		if a.Bool != b.Bool {
			if !a.Bool {
				return -1
			}
			return 1
		}
	}
	return 0
}
