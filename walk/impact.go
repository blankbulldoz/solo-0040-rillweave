package walk

import (
	"cmp"
	"math/bits"
	"slices"

	"rillweave/graph"
	"rillweave/index"
)

// MaxSources bounds one attribution request. Propagation keeps one source
// bitset per reached vertex, so this limit bounds the shared expansion, the
// number of evidence chains and the size of a merged consumer entry together.
const MaxSources = 64

const (
	DrivenSingle = "single"
	DrivenShared = "shared"
)

type ImpactRequest struct {
	// Sources are the vertices whose changes are being assessed. They are
	// propagated at once and each is attributed separately.
	Sources []string `json:"sources"`
	// MaxHops is the propagation horizon. Omitting it uses MaxHops; an explicit
	// zero is a valid request that reports no consumers.
	MaxHops *int `json:"max_hops,omitempty"`
	// Limit bounds the returned consumer window. It does not bound Matched or
	// the per-source scope totals.
	Limit int `json:"limit,omitempty"`
	Selection
}

// Chain is one deterministic witness from a change source to a consumer.
// Vertices has one more entry than Arcs; each arc joins the neighbouring
// vertices in traversal order.
type Chain struct {
	Vertices []string `json:"vertices"`
	Arcs     []string `json:"arcs"`
}

// Attribution names one change source and the cheapest witness by which it
// reaches the consumer.
type Attribution struct {
	Source   string `json:"source"`
	Hops     int    `json:"hops"`
	Evidence Chain  `json:"evidence"`
}

// ImpactConsumer merges every change source that reaches one consumer.
// DrivenBy is "single" when exactly one source reaches it and "shared" when
// several do, so a caller can separate a local edit from a joint obligation.
// AlsoSource marks a consumer that is itself one of the requested change
// sources, which is how a cycle cluster shows that its members force each
// other to act.
type ImpactConsumer struct {
	Vertex     string        `json:"vertex"`
	Hops       int           `json:"hops"`
	DrivenBy   string        `json:"driven_by"`
	AlsoSource bool          `json:"also_source,omitempty"`
	Sources    []Attribution `json:"sources"`
}

// SourceScope summarizes one change source over all consumers, including those
// outside the bounded consumer window.
type SourceScope struct {
	Source     string `json:"source"`
	Consumers  int    `json:"consumers"`
	NearestHop int    `json:"nearest_hop"`
}

type ImpactResult struct {
	Sources      []string         `json:"sources"`
	Direction    string           `json:"direction"`
	Horizon      int              `json:"horizon"`
	SourceScope  []SourceScope    `json:"source_scope"`
	Consumers    []ImpactConsumer `json:"consumers"`
	Matched      int              `json:"matched"`
	Truncated    bool             `json:"truncated"`
	Attributions int              `json:"attributions"`
	Scanned      int              `json:"scanned"`
	Budget       int              `json:"budget"`
}

type sourceBits []uint64

func (b sourceBits) add(index int) {
	b[index>>6] |= uint64(1) << (index & 63)
}

func (b sourceBits) count() int {
	total := 0
	for _, word := range b {
		total += bits.OnesCount64(word)
	}
	return total
}

// ones lists the set source indexes from least to most significant.
func (b sourceBits) ones() []int {
	indexes := make([]int, 0, len(b))
	for word, value := range b {
		for value != 0 {
			bit := value & -value
			indexes = append(indexes, word*64+bits.TrailingZeros64(bit))
			value ^= bit
		}
	}
	return indexes
}

type arrival struct {
	from  string
	arc   string
	depth int
}

type consumer struct {
	vertex string
	hops   int
	bits   sourceBits
}

// Impact propagates every selected change source at once over the chosen
// direction and arc kinds. A vertex carries the set of sources that have
// already reached it, so a cycle keeps relaying a source around its group but
// each source is expanded from a vertex at most once: the second arrival finds
// no new bit and is dropped. One first-arrival parent per source and vertex is
// enough to rebuild the reported chains afterwards, so the search never exposes
// every source/vertex pair as an intermediate result.
//
// Every vertex another source reaches is a consumer, including a requested
// change source that a peer reaches: that shows the mutual obligation inside a
// cycle cluster, and AlsoSource marks it. A source never reports reaching
// itself. Consumers are ordered by nearest hop count and then vertex ID, so a
// source that its peers reach sorts by how close that forcing change was. The
// bounded window costs no extra budget, and Matched, Attributions and
// SourceScope describe the full propagation rather than the window.
func Impact(g *graph.Graph, a *index.Adjacency, q ImpactRequest) (ImpactResult, error) {
	if len(q.Sources) == 0 || len(q.Sources) > MaxSources {
		return ImpactResult{}, graph.Fail(graph.Invalid, "impact requires 1..%d change sources", MaxSources)
	}
	limit, err := Limit(q.Limit)
	if err != nil {
		return ImpactResult{}, err
	}
	sources := slices.Clone(q.Sources)
	slices.Sort(sources)
	origin := make(map[string]int, len(sources))
	for index, source := range sources {
		if index != 0 && sources[index-1] == source {
			return ImpactResult{}, graph.Fail(graph.Invalid, "change source %q is repeated", source)
		}
		if err := requireVertex(g, source); err != nil {
			return ImpactResult{}, err
		}
		origin[source] = index
	}
	horizon := MaxHops
	if q.MaxHops != nil {
		if err := hops(*q.MaxHops); err != nil {
			return ImpactResult{}, err
		}
		horizon = *q.MaxHops
	}
	t, err := topologyFor(g, a, q.Selection)
	if err != nil {
		return ImpactResult{}, err
	}

	words := (len(sources) + 63) / 64
	labels := make(map[string]sourceBits, len(g.Vertices))
	frontier := make(map[string]sourceBits, len(sources))
	for index, source := range sources {
		known := labels[source]
		if known == nil {
			known = make(sourceBits, words)
			labels[source] = known
		}
		known.add(index)
		fresh := frontier[source]
		if fresh == nil {
			fresh = make(sourceBits, words)
			frontier[source] = fresh
		}
		fresh.add(index)
	}

	parents := make([]map[string]arrival, len(sources))
	scope := make([]SourceScope, len(sources))
	for index, source := range sources {
		scope[index] = SourceScope{Source: source}
	}
	for depth := 1; depth <= horizon && len(frontier) != 0; depth++ {
		next := make(map[string]sourceBits)
		for _, vertex := range graph.Keys(frontier) {
			fresh := frontier[vertex]
			err := t.visit(vertex, t.direction, func(arc graph.Arc, target string) error {
				known := labels[target]
				if known == nil {
					known = make(sourceBits, words)
					labels[target] = known
				}
				novel := make(sourceBits, words)
				changed := false
				for word := range novel {
					novel[word] = fresh[word] &^ known[word]
					if novel[word] != 0 {
						changed = true
					}
				}
				if !changed {
					return nil
				}
				for word := range known {
					known[word] |= novel[word]
				}
				arriving := next[target]
				if arriving == nil {
					arriving = make(sourceBits, words)
					next[target] = arriving
				}
				for word := range arriving {
					arriving[word] |= novel[word]
				}
				for _, index := range novel.ones() {
					if parents[index] == nil {
						parents[index] = make(map[string]arrival)
					}
					if _, recorded := parents[index][target]; !recorded {
						parents[index][target] = arrival{vertex, arc.ID, depth}
					}
					scope[index].Consumers++
					if scope[index].NearestHop == 0 {
						scope[index].NearestHop = depth
					}
				}
				return nil
			})
			if err != nil {
				return ImpactResult{}, err
			}
		}
		frontier = next
	}

	consumers := make([]consumer, 0)
	attributions := 0
	for _, vertex := range graph.Keys(labels) {
		self, isSource := origin[vertex]
		reaching := make(sourceBits, words)
		for _, index := range labels[vertex].ones() {
			if isSource && index == self {
				continue
			}
			reaching.add(index)
		}
		if reaching.count() == 0 {
			continue
		}
		nearest := 0
		for _, index := range reaching.ones() {
			if step, ok := parents[index][vertex]; ok && (nearest == 0 || step.depth < nearest) {
				nearest = step.depth
			}
		}
		consumers = append(consumers, consumer{vertex, nearest, reaching})
		attributions += reaching.count()
	}
	slices.SortFunc(consumers, func(a, b consumer) int {
		return cmp.Or(cmp.Compare(a.hops, b.hops), cmp.Compare(a.vertex, b.vertex))
	})

	result := ImpactResult{
		Sources:      sources,
		Direction:    string(t.direction),
		Horizon:      horizon,
		SourceScope:  scope,
		Consumers:    make([]ImpactConsumer, 0, min(limit, len(consumers))),
		Matched:      len(consumers),
		Attributions: attributions,
		Scanned:      t.scanned,
		Budget:       t.allowance,
	}
	for _, row := range consumers {
		if len(result.Consumers) == limit {
			break
		}
		driven := DrivenShared
		if row.bits.count() == 1 {
			driven = DrivenSingle
		}
		evidence := make([]Attribution, 0, row.bits.count())
		for _, index := range row.bits.ones() {
			chain := trace(row.vertex, sources[index], parents[index])
			evidence = append(evidence, Attribution{sources[index], len(chain.Arcs), chain})
		}
		_, also := origin[row.vertex]
		result.Consumers = append(result.Consumers,
			ImpactConsumer{row.vertex, row.hops, driven, also, evidence})
	}
	result.Truncated = result.Matched > limit
	return result, nil
}

// trace reverses the recorded first-arrival parents into a forward witness.
func trace(vertex, source string, parent map[string]arrival) Chain {
	vertices := []string{vertex}
	arcs := []string{}
	for current := vertex; current != source; {
		step, ok := parent[current]
		if !ok {
			break
		}
		arcs = append(arcs, step.arc)
		current = step.from
		vertices = append(vertices, current)
	}
	slices.Reverse(vertices)
	slices.Reverse(arcs)
	return Chain{vertices, arcs}
}
