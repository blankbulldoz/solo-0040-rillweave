package walk

// dominators builds the immediate-dominator tree of a directed graph rooted at
// one artificial root. Real vertices are numbered 1..n and the caller passes
// only vertices reachable from the root, so a single DFS covers the whole
// structure. Number zero means "no vertex", so the root is numbered n+1.
//
// The construction is Lengauer-Tarjan. Its result is independent of the order
// in which equal sets of successors are visited; the impact query sorts
// successor and predecessor lists by vertex number, and numbers vertices by
// identity, so a replay produces the same tree.
type dominators struct {
	n    int
	root int

	succ [][]int
	pred [][]int

	parent   []int
	semi     []int
	idom     []int
	ancestor []int
	label    []int
	dfn      []int
	order    []int
	bucket   [][]int
	count    int
}

func newDominators(n int, succ, pred [][]int, root int) *dominators {
	d := &dominators{n: n, root: root, succ: succ, pred: pred}
	size := n + 2
	d.parent = make([]int, size)
	d.semi = make([]int, size)
	d.idom = make([]int, size)
	d.ancestor = make([]int, size)
	d.label = make([]int, size)
	d.dfn = make([]int, size)
	d.order = make([]int, size)
	d.bucket = make([][]int, size)
	d.dfs(root)
	for i := d.count; i >= 2; i-- {
		w := d.order[i]
		for _, v := range d.pred[w] {
			u := d.eval(v)
			if d.semi[u] < d.semi[w] {
				d.semi[w] = d.semi[u]
			}
		}
		pw := d.parent[w]
		d.bucket[d.order[d.semi[w]]] = append(d.bucket[d.order[d.semi[w]]], w)
		d.link(pw, w)
		for _, v := range d.bucket[pw] {
			u := d.eval(v)
			if d.semi[u] < d.semi[v] {
				d.idom[v] = u
			} else {
				d.idom[v] = pw
			}
		}
		d.bucket[pw] = nil
	}
	for i := 2; i <= d.count; i++ {
		w := d.order[i]
		if d.idom[w] != d.order[d.semi[w]] {
			d.idom[w] = d.idom[d.idom[w]]
		}
	}
	d.idom[root] = 0
	return d
}

func (d *dominators) dfs(v int) {
	d.count++
	d.dfn[v] = d.count
	d.order[d.count] = v
	d.semi[v] = d.count
	d.label[v] = v
	for _, w := range d.succ[v] {
		if d.dfn[w] == 0 {
			d.parent[w] = v
			d.dfs(w)
		}
	}
}

func (d *dominators) link(parent, child int) {
	d.ancestor[child] = parent
}

func (d *dominators) eval(v int) int {
	if d.ancestor[v] == 0 {
		return v
	}
	d.compress(v)
	return d.label[v]
}

func (d *dominators) compress(v int) {
	a := d.ancestor[v]
	if d.ancestor[a] == 0 {
		return
	}
	d.compress(a)
	if d.semi[d.label[a]] < d.semi[d.label[v]] {
		d.label[v] = d.label[a]
	}
	d.ancestor[v] = d.ancestor[a]
}

// intervals returns half-open dominator-subtree intervals [tin,tout). A vertex
// c is a strict dominator of t exactly when tin[c] <= tin[t] < tout[c].
func (d *dominators) intervals() (tin, tout []int) {
	size := d.n + 2
	children := make([][]int, size)
	for v := 1; v <= d.n; v++ {
		if p := d.idom[v]; p != 0 {
			children[p] = append(children[p], v)
		}
	}
	tin = make([]int, size)
	tout = make([]int, size)
	clock := 0
	var walk func(int)
	walk = func(v int) {
		clock++
		tin[v] = clock
		for _, w := range children[v] {
			walk(w)
		}
		tout[v] = clock + 1
	}
	walk(d.root)
	return tin, tout
}
