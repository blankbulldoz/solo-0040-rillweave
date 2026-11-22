package walk

import (
	"slices"
	"strconv"

	"rillweave/catalog"
	"rillweave/graph"
	"rillweave/index"
)

// PropertyFilter is one typed equality condition used to narrow a vertex
// cohort. A property that is missing or holds another scalar type never
// matches, mirroring the lookup posting semantics.
type PropertyFilter struct {
	Property string       `json:"property"`
	Value    graph.Scalar `json:"value"`
}

// CensusRequest selects a vertex cohort and an arc-kind set, then asks for the
// in/out degree bucket of every cohort member.
type CensusRequest struct {
	Kind     string           `json:"kind"`
	Filters  []PropertyFilter `json:"filters,omitempty"`
	ArcKinds []string         `json:"arc_kinds"`
	Distinct bool             `json:"distinct,omitempty"`
	Budget   int              `json:"budget,omitempty"`
	Limit    int              `json:"limit,omitempty"`
}

// CensusRow is the bucket placement of one cohort vertex.
type CensusRow struct {
	Vertex       string `json:"vertex"`
	InDegree     int    `json:"in_degree"`
	OutDegree    int    `json:"out_degree"`
	InNeighbors  int    `json:"in_neighbors"`
	OutNeighbors int    `json:"out_neighbors"`
	Bucket       string `json:"bucket"`
}

// BucketCount is the size of one degree combination.
type BucketCount struct {
	Bucket string `json:"bucket"`
	In     int    `json:"in"`
	Out    int    `json:"out"`
	Count  int    `json:"count"`
}

// CensusResult keeps every cohort member, including zero-degree vertices, and
// an aggregate table over the same rows.
type CensusResult struct {
	Kind       string        `json:"kind"`
	ArcKinds   []string      `json:"arc_kinds"`
	Distinct   bool          `json:"distinct"`
	CohortSize int           `json:"cohort_size"`
	Rows       []CensusRow   `json:"rows"`
	Buckets    []BucketCount `json:"buckets"`
	Scanned    int           `json:"scanned"`
	Truncated  bool          `json:"truncated"`
}

// EvidenceRequest resolves the members of one bucket of a fixed view into a
// small, reproducible set of witnessing arc identities.
type EvidenceRequest struct {
	Kind     string           `json:"kind"`
	Filters  []PropertyFilter `json:"filters,omitempty"`
	ArcKinds []string         `json:"arc_kinds"`
	Bucket   string           `json:"bucket"`
	Distinct bool             `json:"distinct,omitempty"`
	Limit    int              `json:"limit,omitempty"`
	Budget   int              `json:"budget,omitempty"`
}

// EvidenceItem pairs one bucket member with the arcs that place it there.
type EvidenceItem struct {
	Vertex    string   `json:"vertex"`
	InArcs    []string `json:"in_arcs"`
	OutArcs   []string `json:"out_arcs"`
	InDegree  int      `json:"in_degree"`
	OutDegree int      `json:"out_degree"`
}

// EvidenceResult is the bounded witness window for a bucket.
type EvidenceResult struct {
	Bucket    string         `json:"bucket"`
	Matched   int            `json:"matched"`
	Items     []EvidenceItem `json:"items"`
	Truncated bool           `json:"truncated"`
	Scanned   int            `json:"scanned"`
}

func bucketLabel(in, out int) string {
	return strconv.Itoa(in) + "x" + strconv.Itoa(out)
}

// parseBucket accepts the same "inxout" labels emitted by a census.
func parseBucket(label string) (in, out int, err error) {
	mark := -1
	for i, r := range label {
		if r == 'x' {
			if mark != -1 {
				return 0, 0, graph.Fail(graph.Invalid, "bucket must look like inxout, e.g. 0x1")
			}
			mark = i
		}
	}
	if mark <= 0 || mark == len(label)-1 {
		return 0, 0, graph.Fail(graph.Invalid, "bucket must look like inxout, e.g. 0x1")
	}
	digits := func(part string) (int, error) {
		n := 0
		for _, r := range part {
			if r < '0' || r > '9' {
				return 0, graph.Fail(graph.Invalid, "bucket degrees must be non-negative integers")
			}
			n = n*10 + int(r-'0')
		}
		return n, nil
	}
	if in, err = digits(label[:mark]); err != nil {
		return 0, 0, err
	}
	out, err = digits(label[mark+1:])
	return in, out, err
}

type censusPlan struct {
	kinds     map[string]bool
	allowance int
	distinct  bool
	scanned   int
}

func prepareCensus(g *graph.Graph, kind string, filters []PropertyFilter, arcKinds []string, distinct bool, budget int) (censusPlan, error) {
	if _, ok := g.Schema.Vertices[kind]; !ok {
		return censusPlan{}, graph.Fail(graph.Invalid, "unknown vertex kind %q", kind)
	}
	if len(filters) > graph.MaxProps {
		return censusPlan{}, graph.Fail(graph.Invalid, "at most %d property filters are allowed", graph.MaxProps)
	}
	seenFilter := make(map[string]bool, len(filters))
	for _, filter := range filters {
		if seenFilter[filter.Property] {
			return censusPlan{}, graph.Fail(graph.Invalid, "duplicate property filter %q", filter.Property)
		}
		seenFilter[filter.Property] = true
		if err := catalog.LookupRule(g.Schema, kind, filter.Property, filter.Value); err != nil {
			return censusPlan{}, err
		}
	}
	if len(arcKinds) == 0 || len(arcKinds) > graph.MaxRules {
		return censusPlan{}, graph.Fail(graph.Invalid, "1..%d arc kinds are required", graph.MaxRules)
	}
	kinds := make(map[string]bool, len(arcKinds))
	for _, arcKind := range arcKinds {
		if _, ok := g.Schema.Arcs[arcKind]; !ok {
			return censusPlan{}, graph.Fail(graph.Invalid, "unknown arc kind %q", arcKind)
		}
		if kinds[arcKind] {
			return censusPlan{}, graph.Fail(graph.Invalid, "duplicate arc kind %q", arcKind)
		}
		kinds[arcKind] = true
	}
	if budget == 0 {
		budget = MaxBudget
	}
	if budget < 1 || budget > MaxBudget {
		return censusPlan{}, graph.Fail(graph.Invalid, "budget must be in [1,%d]", MaxBudget)
	}
	return censusPlan{kinds: kinds, allowance: budget, distinct: distinct}, nil
}

// cohort returns the sorted IDs of vertices of the requested kind that satisfy
// every typed equality filter. Each vertex is evaluated once, so a self-loop
// never inflates the population count.
func cohort(g *graph.Graph, kind string, filters []PropertyFilter) []string {
	members := make([]string, 0)
	for _, id := range graph.Keys(g.Vertices) {
		vertex := g.Vertices[id]
		if vertex.Kind != kind {
			continue
		}
		matches := true
		for _, filter := range filters {
			value, present := vertex.Properties[filter.Property]
			if !present || value != filter.Value {
				matches = false
				break
			}
		}
		if matches {
			members = append(members, id)
		}
	}
	return members
}

type degree struct {
	in, out      int
	inNeighbors  map[string]struct{}
	outNeighbors map[string]struct{}
	inArcs       []string
	outArcs      []string
}

// charge mirrors topology.visit: every candidate arc on either posting costs
// one unit, including candidates rejected by the kind selection. Exhaustion
// fails the whole query so an unscanned vertex can never look zero-degree.
func (p *censusPlan) charge() error {
	if p.scanned == p.allowance {
		return graph.Fail(graph.Budget, "census budget exhausted after %d candidate arcs", p.scanned)
	}
	p.scanned++
	return nil
}

// measure walks the in and out postings of one vertex separately. A self-loop
// sits on both postings and therefore contributes once to each direction, but
// it remains one arc identity and one distinct neighbor.
func (p *censusPlan) measure(a *index.Adjacency, id string, d *degree, keepArcs bool) error {
	if p.distinct {
		d.inNeighbors = make(map[string]struct{})
		d.outNeighbors = make(map[string]struct{})
	}
	err := a.Visit(id, index.Out, func(arc graph.Arc, next string) error {
		if err := p.charge(); err != nil {
			return err
		}
		if !p.kinds[arc.Kind] {
			return nil
		}
		d.out++
		if p.distinct {
			d.outNeighbors[next] = struct{}{}
		}
		if keepArcs {
			d.outArcs = append(d.outArcs, arc.ID)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return a.Visit(id, index.In, func(arc graph.Arc, next string) error {
		if err := p.charge(); err != nil {
			return err
		}
		if !p.kinds[arc.Kind] {
			return nil
		}
		d.in++
		if p.distinct {
			d.inNeighbors[next] = struct{}{}
		}
		if keepArcs {
			d.inArcs = append(d.inArcs, arc.ID)
		}
		return nil
	})
}

func (d degree) distinctCounts(plan *censusPlan) (in, out int) {
	if plan.distinct {
		return len(d.inNeighbors), len(d.outNeighbors)
	}
	return d.in, d.out
}

// Census classifies every cohort member by its selected in/out degree
// combination. Budget exhaustion rejects the whole query: an unscanned vertex
// is never silently reported as zero-degree.
func Census(g *graph.Graph, a *index.Adjacency, q CensusRequest) (CensusResult, error) {
	plan, err := prepareCensus(g, q.Kind, q.Filters, q.ArcKinds, q.Distinct, q.Budget)
	if err != nil {
		return CensusResult{}, err
	}
	limit, err := Limit(q.Limit)
	if err != nil {
		return CensusResult{}, err
	}
	members := cohort(g, q.Kind, q.Filters)
	result := CensusResult{
		Kind: q.Kind, ArcKinds: slices.Clone(q.ArcKinds), Distinct: q.Distinct,
		CohortSize: len(members), Rows: []CensusRow{}, Buckets: []BucketCount{},
	}
	totals := make(map[string]*BucketCount)
	for _, id := range members {
		var d degree
		if err := plan.measure(a, id, &d, false); err != nil {
			return CensusResult{}, err
		}
		in, out := d.distinctCounts(&plan)
		label := bucketLabel(in, out)
		count, ok := totals[label]
		if !ok {
			count = &BucketCount{Bucket: label, In: in, Out: out}
			totals[label] = count
		}
		count.Count++
		if len(result.Rows) < limit {
			result.Rows = append(result.Rows, CensusRow{
				Vertex: id, InDegree: d.in, OutDegree: d.out,
				InNeighbors: len(d.inNeighbors), OutNeighbors: len(d.outNeighbors),
				Bucket: label,
			})
		}
	}
	for _, label := range graph.Keys(totals) {
		result.Buckets = append(result.Buckets, *totals[label])
	}
	slices.SortFunc(result.Buckets, func(x, y BucketCount) int {
		if x.In != y.In {
			return x.In - y.In
		}
		return x.Out - y.Out
	})
	result.Truncated = len(members) > limit
	result.Scanned = plan.scanned
	return result, nil
}

// Evidence re-runs the cohort selection and returns arc witnesses for members
// of exactly one bucket. A budget failure still rejects the request instead of
// returning a partial witness list.
func Evidence(g *graph.Graph, a *index.Adjacency, q EvidenceRequest) (EvidenceResult, error) {
	plan, err := prepareCensus(g, q.Kind, q.Filters, q.ArcKinds, q.Distinct, q.Budget)
	if err != nil {
		return EvidenceResult{}, err
	}
	wantIn, wantOut, err := parseBucket(q.Bucket)
	if err != nil {
		return EvidenceResult{}, err
	}
	limit, err := Limit(q.Limit)
	if err != nil {
		return EvidenceResult{}, err
	}
	members := cohort(g, q.Kind, q.Filters)
	result := EvidenceResult{Bucket: bucketLabel(wantIn, wantOut), Items: []EvidenceItem{}}
	for _, id := range members {
		var d degree
		if err := plan.measure(a, id, &d, true); err != nil {
			return EvidenceResult{}, err
		}
		in, out := d.distinctCounts(&plan)
		if in != wantIn || out != wantOut {
			continue
		}
		result.Matched++
		if len(result.Items) < limit {
			result.Items = append(result.Items, EvidenceItem{
				Vertex: id, InArcs: d.inArcs, OutArcs: d.outArcs,
				InDegree: d.in, OutDegree: d.out,
			})
		}
	}
	result.Truncated = result.Matched > limit
	result.Scanned = plan.scanned
	return result, nil
}
