package walk

import (
	"rillweave/graph"
	"rillweave/index"
)

// ReachPageRequest starts a resumable minimum-hop traversal. The selection and
// hop bound are fixed for the whole session; each continuation only carries the
// opaque handle and the arc-inspection allowance for that page.
type ReachPageRequest struct {
	Root    string `json:"root,omitempty"`
	MaxHops int    `json:"max_hops,omitempty"`
	Selection
}

// ReachContinue advances an existing page by at most Budget candidate arcs.
// Budget zero or omitted reuses the session budget.
type ReachContinue struct {
	Budget int `json:"budget,omitempty"`
}

// ReachPage is one segment of a resumable traversal. Complete means the
// frontier is exhausted (or the hop bound was reached), not merely that the
// page allowance ran out. Scanned counts only this page's candidate arcs.
type ReachPage struct {
	Distances []Distance `json:"distances"`
	Scanned   int        `json:"scanned"`
	Complete  bool       `json:"complete"`
}

// frontierCursor is the queue head: the next vertex row to scan and the exact
// posting offsets inside that row. A row stays at the head until every one of
// its candidate arcs has been inspected, so a layer never closes mid-row.
type frontierCursor struct {
	vertex    string
	hops      int
	outOffset int
	inOffset  int
}

// ReachCursor holds all server-side traversal state. The client only keeps an
// opaque handle, so a frontier of any width fits without a wide client
// cursor.
type ReachCursor struct {
	root      string
	maxHops   int
	selection Selection

	// firstPage delivers the root at zero hops before any arc is inspected.
	firstPage bool
	queue     []frontierCursor
	seen      map[string]bool
	scanned   int
	done      bool
}

// NewReachCursor validates the request against the chosen state and opens a
// session at the root. The root itself is the first proven distance.
func NewReachCursor(g *graph.Graph, a *index.Adjacency, q ReachPageRequest) (*ReachCursor, error) {
	if err := hops(q.MaxHops); err != nil {
		return nil, err
	}
	if _, err := topologyFor(g, a, q.Selection); err != nil {
		return nil, err
	}
	if err := requireVertex(g, q.Root); err != nil {
		return nil, err
	}
	selection := q.Selection
	if selection.Budget == 0 {
		selection.Budget = MaxBudget
	}
	return &ReachCursor{
		root:      q.Root,
		maxHops:   q.MaxHops,
		selection: selection,
		firstPage: true,
		queue:     []frontierCursor{{vertex: q.Root}},
		seen:      map[string]bool{q.Root: true},
	}, nil
}

// Done reports whether the frontier has been fully consumed.
func (c *ReachCursor) Done() bool { return c.done }

// Scanned reports the cumulative number of candidate arc inspections.
func (c *ReachCursor) Scanned() int { return c.scanned }

func pageBudget(session, requested int) (int, error) {
	if requested == 0 {
		requested = session
	}
	if requested < 1 || requested > MaxBudget {
		return 0, graph.Fail(graph.Invalid, "expansion budget must be in [1,%d]", MaxBudget)
	}
	return requested, nil
}

// Advance scans at most the requested number of candidate arcs and returns the
// newly proven distances. Exhausting the allowance is a clean pause: the row
// offsets are retained and another Advance continues exactly where this page
// stopped, so stitching pages reproduces a full Reach query.
func (c *ReachCursor) Advance(g *graph.Graph, a *index.Adjacency, requested int) (ReachPage, error) {
	page := ReachPage{Distances: []Distance{}, Complete: c.done}
	if c.done {
		return page, nil
	}
	allowance, err := pageBudget(c.selection.Budget, requested)
	if err != nil {
		return ReachPage{}, err
	}
	t, err := topologyFor(g, a, c.selection)
	if err != nil {
		return ReachPage{}, err
	}
	if c.firstPage {
		// The root at zero hops is a proven distance costing no inspection.
		// Scanning starts on the next page so the call that opens a session
		// never spends part of a page allowance before the client sees it.
		c.firstPage = false
		page.Distances = append(page.Distances, Distance{Vertex: c.root, Hops: 0})
		if c.maxHops == 0 {
			c.done = true
			page.Complete = true
		}
		return page, nil
	}
	for len(c.queue) != 0 {
		head := c.queue[0]
		// A vertex at the hop bound is a delivered leaf: its row is never
		// expanded, so the next layer cannot begin past the bound.
		if head.hops == c.maxHops {
			c.queue = c.queue[1:]
			continue
		}
		halted := false
		nextOut, nextIn, scanErr := a.VisitWindow(
			head.vertex, t.direction, head.outOffset, head.inOffset,
			func(graph.Arc, string) (bool, error) {
				if t.scanned == allowance {
					halted = true
					return true, nil
				}
				return false, nil
			},
			func(arc graph.Arc, next string) error {
				t.scanned++
				if len(t.kinds) != 0 && !t.kinds[arc.Kind] {
					return nil
				}
				if !c.seen[next] {
					c.seen[next] = true
					page.Distances = append(page.Distances, Distance{next, head.hops + 1})
					c.queue = append(c.queue, frontierCursor{vertex: next, hops: head.hops + 1})
				}
				return nil
			},
		)
		c.scanned += t.scanned
		if scanErr != nil {
			return ReachPage{}, scanErr
		}
		if halted {
			// Keep the same row at the queue head with its exact offsets.
			c.queue[0].outOffset = nextOut
			c.queue[0].inOffset = nextIn
			page.Scanned = t.scanned
			return page, nil
		}
		// Row fully scanned: only now may the layer advance past this vertex.
		c.queue = c.queue[1:]
	}
	c.done = true
	page.Complete = true
	page.Scanned = t.scanned
	return page, nil
}
