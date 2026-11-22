package compress

import (
	"container/heap"
	"math"
	"slices"
	"testing"

	"rillweave/graph"
	"rillweave/index"
)

// oraclePath is an independent, unconstrained Dijkstra over the original
// selected subgraph, used to prove the compressed summary returns the true
// minimum forward cost for every agreed cross-cluster endpoint pair.
type oracleLabel struct {
	vertex string
	cost   int64
	arcs   []string
	index  int
}

type oracleHeap []*oracleLabel

func (h oracleHeap) Len() int { return len(h) }
func (h oracleHeap) Less(i, j int) bool {
	if h[i].cost != h[j].cost {
		return h[i].cost < h[j].cost
	}
	if len(h[i].arcs) != len(h[j].arcs) {
		return len(h[i].arcs) < len(h[j].arcs)
	}
	return slices.Compare(h[i].arcs, h[j].arcs) < 0
}
func (h oracleHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index, h[j].index = j, i
}
func (h *oracleHeap) Push(x any) { l := x.(*oracleLabel); l.index = len(*h); *h = append(*h, l) }
func (h *oracleHeap) Pop() any {
	old := *h
	n := len(old)
	l := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return l
}

func oracleShortest(g *graph.Graph, kinds map[string]bool, from, to string) (int64, []string, bool) {
	out := index.BuildAdjacency(g)
	root := &oracleLabel{vertex: from, arcs: []string{}}
	dist := map[string]*oracleLabel{from: root}
	queue := &oracleHeap{root}
	heap.Init(queue)
	for queue.Len() != 0 {
		cur := heap.Pop(queue).(*oracleLabel)
		if dist[cur.vertex] != cur {
			continue
		}
		if cur.vertex == to {
			return cur.cost, cur.arcs, true
		}
		_ = out.Visit(cur.vertex, index.Out, func(arc graph.Arc, next string) error {
			if len(kinds) > 0 && !kinds[arc.Kind] {
				return nil
			}
			candidate := &oracleLabel{
				vertex: next, cost: cur.cost + arc.Cost,
				arcs: append(slices.Clone(cur.arcs), arc.ID),
			}
			prior, ok := dist[next]
			better := !ok || candidate.cost < prior.cost ||
				candidate.cost == prior.cost &&
					(len(candidate.arcs) < len(prior.arcs) ||
						len(candidate.arcs) == len(prior.arcs) && slices.Compare(candidate.arcs, prior.arcs) < 0)
			if better {
				dist[next] = candidate
				heap.Push(queue, candidate)
			}
			return nil
		})
	}
	return math.MaxInt64, nil, false
}

func TestSummaryMatchesOriginalShortestPaths(t *testing.T) {
	g := testGraph(t)
	endpoints := []string{"a1", "a3", "b1", "b2", "c1", "c2", "lone"}
	network, doc := buildTestNetwork(t, []string{"join", "guide"}, endpoints)
	kinds := map[string]bool{"join": true, "guide": true}

	checked, found, noRoute := 0, 0, 0
	for _, from := range endpoints {
		for _, to := range endpoints {
			if from == to || network.clusterOf[from] == network.clusterOf[to] {
				continue // exact contract covers cross-cluster pairs only
			}
			checked++
			wantCost, wantArcs, wantFound := oracleShortest(g, kinds, from, to)
			answer, err := network.Route(RouteRequest{From: from, To: to})
			if err != nil {
				t.Fatalf("Route %s->%s: %v", from, to, err)
			}
			if !answer.Exact {
				t.Fatalf("%s->%s must be exact", from, to)
			}
			if wantFound != answer.Found {
				t.Fatalf("%s->%s found mismatch: oracle=%v summary=%v", from, to, wantFound, answer.Found)
			}
			if !wantFound {
				noRoute++
				continue
			}
			found++
			if answer.Cost != wantCost {
				t.Fatalf("%s->%s cost: summary=%d oracle=%d arcs=%v", from, to, answer.Cost, wantCost, answer.Arcs)
			}
			// Deterministic tie-break must reproduce the original arc sequence.
			if !slices.Equal(answer.Arcs, wantArcs) {
				t.Fatalf("%s->%s witness:\n summary=%v\n oracle =%v", from, to, answer.Arcs, wantArcs)
			}
			verify := network.Replay(answer.Arcs)
			if !verify.Valid || verify.Cost != wantCost {
				t.Fatalf("%s->%s replay failed: %+v", from, to, verify)
			}
		}
	}
	if checked == 0 || found == 0 || noRoute == 0 {
		t.Fatalf("expected a mix of reachable and unreachable pairs: checked=%d found=%d noroute=%d",
			checked, found, noRoute)
	}
	if doc.Stats.RetainedArcs >= doc.Stats.OriginalArcs {
		t.Fatalf("summary must retain fewer arcs: %+v", doc.Stats)
	}
	t.Logf("verified %d cross-cluster pairs (%d reachable, %d no-route); retained %d/%d arcs",
		checked, found, noRoute, doc.Stats.RetainedArcs, doc.Stats.OriginalArcs)
}

func TestWitnessPoolIsDeduplicated(t *testing.T) {
	_, doc := buildTestNetwork(t, []string{"join", "guide"},
		[]string{"a1", "a3", "b1", "b2", "c1", "c2"})
	seen := map[string]bool{}
	for _, witness := range doc.Witnesses {
		key := seqKey(witness.ArcIDs)
		if seen[key] {
			t.Fatalf("witness sequence duplicated in pool: %v", witness.ArcIDs)
		}
		seen[key] = true
	}
	referenced := map[string]bool{}
	for _, edge := range doc.Summaries {
		if !referenced[edge.WitnessID] {
			referenced[edge.WitnessID] = true
			continue
		}
	}
	// Every witness must be referenced by at least one summary edge.
	for _, witness := range doc.Witnesses {
		if !referenced[witness.ID] {
			t.Fatalf("unreferenced witness %q", witness.ID)
		}
	}
}
