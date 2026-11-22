package index

import (
	"math/bits"
	"unsafe"

	"rillweave/graph"
)

// MaxScratch bounds the temporary memory a single composite evaluation may
// allocate. The decoded request and the encoded response are counted
// elsewhere; only per-evaluation working sets are charged against it.
const MaxScratch = 128 << 10

// scratchLimit is a variable so tests can exercise the budget guard.
var scratchLimit = uintptr(MaxScratch)

// MaxLookupConditions bounds the number of typed equality conditions that one
// composite lookup may combine on a single read view.
const MaxLookupConditions = 32

type postingKey struct {
	kind, property string
	value          graph.Scalar
}

// entry pairs a posting member with its global vertex rank. Ranks follow the
// sorted vertex IDs of the materialized revision, so every posting stays in
// deterministic ID order without re-sorting during a query.
type entry struct {
	id   string
	rank int
}

type Postings struct {
	values map[postingKey][]entry
	// order holds every vertex ID of the revision in sorted order. A rank is
	// an index into order, which lets set algebra run on compact bitmaps and
	// lets final results be read back in the original ID ordering.
	order []string
	rank  map[string]int
}

func BuildPostings(g *graph.Graph) *Postings {
	order := graph.Keys(g.Vertices)
	rank := make(map[string]int, len(order))
	for i, id := range order {
		rank[id] = i
	}
	p := &Postings{
		values: make(map[postingKey][]entry),
		order:  order,
		rank:   rank,
	}
	// Iterating the sorted order keeps every individual posting sorted by ID,
	// preserving the single-condition window contract.
	for _, id := range order {
		v := g.Vertices[id]
		for property, value := range v.Properties {
			key := postingKey{v.Kind, property, value}
			p.values[key] = append(p.values[key], entry{id, rank[id]})
		}
	}
	return p
}

type Posting struct {
	IDs       []string `json:"ids"`
	Matched   int      `json:"matched"`
	Truncated bool     `json:"truncated"`
}

// Equal resolves one typed equality condition against its immutable posting.
// Missing properties never enter a posting, so an absent value does not match
// any present scalar, including the empty text value.
func (p *Postings) Equal(kind, property string, value graph.Scalar, limit int) Posting {
	entries := p.values[postingKey{kind, property, value}]
	window := min(limit, len(entries))
	ids := make([]string, window)
	for i := range ids {
		ids[i] = entries[i].id
	}
	return Posting{
		IDs:     ids,
		Matched: len(entries), Truncated: len(entries) > limit,
	}
}

// Match is one validated typed equality condition of a composite lookup.
type Match struct {
	Kind     string       `json:"kind"`
	Property string       `json:"property"`
	Value    graph.Scalar `json:"value"`
}

// Operation combines the running result with the next condition's posting.
type Operation string

const (
	Union     Operation = "union"
	Intersect Operation = "intersect"
	Subtract  Operation = "subtract"
)

func (op Operation) Valid() bool {
	return op == Union || op == Intersect || op == Subtract
}

// Term pairs a condition with the operation applied before it. The first
// term seeds the working set; its operation is ignored.
type Term struct {
	Match
	Op Operation `json:"op,omitempty"`
}

// bitmap is a dense vertex-identity set indexed by Postings rank. Two
// bitmaps are the only large working structures an evaluation keeps live, so
// scratch memory is O(vertex count) and independent of how many of the 32
// conditions run.
type bitmap struct {
	words []uint64
}

func newBitmap(vertices int) bitmap {
	return bitmap{words: make([]uint64, (vertices+63)/64)}
}

func (b bitmap) set(rank int) {
	b.words[rank>>6] |= 1 << (uint(rank) & 63)
}

func (b bitmap) count() int {
	total := 0
	for _, word := range b.words {
		total += bits.OnesCount64(word)
	}
	return total
}

func (b bitmap) unionWith(other bitmap) {
	for i := range b.words {
		b.words[i] |= other.words[i]
	}
}

func (b bitmap) intersectWith(other bitmap) {
	for i := range b.words {
		b.words[i] &= other.words[i]
	}
}

func (b bitmap) subtract(other bitmap) {
	for i := range b.words {
		b.words[i] &^= other.words[i]
	}
}

// scratchBytes estimates the peak temporary footprint of an evaluation: the
// running set and one reusable condition bitmap, plus the bitmap headers.
func scratchBytes(vertices int) uintptr {
	words := (vertices + 63) / 64
	return 2*unsafe.Sizeof(bitmap{}) + uintptr(2*words)*unsafe.Sizeof(uint64(0))
}

// Combine evaluates a left-associative chain of typed equality conditions on
// one read view. Every term and operation must have been validated against
// the catalog first (see catalog.LookupPlan); this function only performs the
// set algebra. Union, intersection and set difference deduplicate by vertex
// identity, and the exact matched count is reported before the ID window is
// truncated by limit.
func (p *Postings) Combine(terms []Term, limit int) (Posting, error) {
	if scratchBytes(len(p.order)) > scratchLimit {
		return Posting{}, graph.Fail(graph.Budget,
			"composite lookup scratch would exceed %d bytes", MaxScratch)
	}

	working := newBitmap(len(p.order))
	candidate := newBitmap(len(p.order))
	for i, term := range terms {
		clear(candidate.words)
		for _, entry := range p.values[postingKey{term.Kind, term.Property, term.Value}] {
			candidate.set(entry.rank)
		}
		if i == 0 {
			copy(working.words, candidate.words)
			continue
		}
		switch term.Op {
		case Union:
			working.unionWith(candidate)
		case Intersect:
			working.intersectWith(candidate)
		case Subtract:
			working.subtract(candidate)
		default:
			return Posting{}, graph.Fail(graph.Invalid, "unknown set operation %q", term.Op)
		}
	}

	matched := working.count()
	ids := make([]string, 0, min(limit, matched))
	for rank, id := range p.order {
		if working.words[rank>>6]&(1<<(uint(rank)&63)) == 0 {
			continue
		}
		if len(ids) == limit {
			break
		}
		ids = append(ids, id)
	}
	return Posting{
		IDs:       ids,
		Matched:   matched,
		Truncated: matched > limit,
	}, nil
}
