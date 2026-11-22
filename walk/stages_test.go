package walk

import (
	"math/rand"
	"testing"
)

func buildDAG(ids []string, edges [][2]string) dagModel {
	pred := map[string]map[string]bool{}
	succ := map[string]map[string]bool{}
	for _, id := range ids {
		pred[id] = map[string]bool{}
		succ[id] = map[string]bool{}
	}
	for _, e := range edges {
		pred[e[1]][e[0]] = true
		succ[e[0]][e[1]] = true
	}
	return dagModel{ids: ids, pred: pred, succ: succ, level: longestPaths(ids, pred)}
}

// bruteStages enumerates every legal stage sequence and returns the minimum.
func bruteStages(d dagModel, width int) int {
	n := len(d.ids)
	index := map[string]int{}
	for i, id := range d.ids {
		index[id] = i
	}
	pred := make([]int, n)
	for i, id := range d.ids {
		for p := range d.pred[id] {
			pred[i] |= 1 << index[p]
		}
	}
	var search func(done, stage int) int
	search = func(done, stage int) int {
		if done == (1<<n)-1 {
			return stage
		}
		ready := []int{}
		for i := 0; i < n; i++ {
			bit := 1 << i
			if done&bit == 0 && pred[i]&done == pred[i] {
				ready = append(ready, i)
			}
		}
		best := n + 1
		var choose func(start, mask, count int)
		choose = func(start, mask, count int) {
			if count > 0 {
				if r := search(done|mask, stage+1); r < best {
					best = r
				}
			}
			if count == width {
				return
			}
			for i := start; i < len(ready); i++ {
				choose(i+1, mask|(1<<ready[i]), count+1)
			}
		}
		choose(0, 0, 0)
		return best
	}
	return search(0, 0)
}

func TestExactMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(20261122))
	for trial := 0; trial < 300; trial++ {
		n := 1 + rng.Intn(10)
		ids := make([]string, n)
		for i := range ids {
			ids[i] = string(rune('a' + i))
		}
		var edges [][2]string
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				if rng.Intn(3) == 0 {
					edges = append(edges, [2]string{ids[i], ids[j]})
				}
			}
		}
		d := buildDAG(ids, edges)
		width := 1 + rng.Intn(4)
		want := bruteStages(d, width)
		got, ok := scheduleExact(d, width)
		if !ok {
			t.Fatalf("trial %d: exact search gave up on %d groups (width=%d edges=%v brute=%d)",
				trial, n, width, edges, want)
		}
		stageOf := map[string]int{}
		maxStage := 0
		for id, stage := range got {
			stageOf[id] = stage
			if stage > maxStage {
				maxStage = stage
			}
		}
		if maxStage != want {
			t.Fatalf("trial %d: got %d stages, brute force says %d (width=%d edges=%v)",
				trial, maxStage, want, width, edges)
		}
		occupancy := map[int]int{}
		for _, stage := range got {
			occupancy[stage]++
			if occupancy[stage] > width {
				t.Fatalf("trial %d: stage %d exceeds width %d", trial, stage, width)
			}
		}
		for _, e := range edges {
			if stageOf[e[0]] >= stageOf[e[1]] {
				t.Fatalf("trial %d: precedence %v violated (%d >= %d)",
					trial, e, stageOf[e[0]], stageOf[e[1]])
			}
		}
	}
}

func TestGreedyIsFeasibleAndBounded(t *testing.T) {
	rng := rand.New(rand.NewSource(77))
	for trial := 0; trial < 200; trial++ {
		n := 1 + rng.Intn(30)
		ids := make([]string, n)
		for i := range ids {
			ids[i] = "v" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		}
		var edges [][2]string
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				if rng.Intn(4) == 0 {
					edges = append(edges, [2]string{ids[i], ids[j]})
				}
			}
		}
		d := buildDAG(ids, edges)
		width := 1 + rng.Intn(5)
		got := scheduleGreedy(d, width)
		occupancy := map[string]int{}
		stageSeen := map[int]bool{}
		for id, stage := range got {
			occupancy[id] = stage
			stageSeen[stage] = true
		}
		widthUsed := map[int]int{}
		for _, stage := range got {
			widthUsed[stage]++
			if widthUsed[stage] > width {
				t.Fatalf("trial %d: width %d exceeded", trial, width)
			}
		}
		for _, e := range edges {
			if occupancy[e[0]] >= occupancy[e[1]] {
				t.Fatalf("trial %d: precedence %v violated", trial, e)
			}
		}
		// No stage number may be skipped.
		for s := 1; s <= len(stageSeen); s++ {
			if !stageSeen[s] {
				t.Fatalf("trial %d: stage %d skipped", trial, s)
			}
		}
	}
}

func TestSubsetEnumeration(t *testing.T) {
	var got []uint32
	forEachSubset(0b1011, 2, func(mask uint32) bool {
		got = append(got, mask)
		return false
	})
	// Size-first lexicographic order on bit positions: singletons, then pairs.
	want := []uint32{0b0001, 0b0011, 0b1001, 0b0010, 0b1010, 0b1000}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("position %d: got %b want %b (full %v)", i, got[i], want[i], got)
		}
	}
	stopped := []uint32{}
	forEachSubset(0b1111, 2, func(mask uint32) bool {
		stopped = append(stopped, mask)
		return mask == 0b0011
	})
	if len(stopped) != 2 || stopped[1] != 0b0011 {
		t.Fatalf("early stop did not hold: %v", stopped)
	}
}
