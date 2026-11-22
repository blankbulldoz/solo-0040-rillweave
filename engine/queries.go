package engine

import (
	"rillweave/catalog"
	"rillweave/graph"
	"rillweave/walk"
)

type ReadResult[T any] struct {
	Graph    string `json:"graph"`
	Revision uint64 `json:"revision"`
	Result   T      `json:"result"`
}

func query[T any](db *Database, name, handle string, f func(*state) (T, error)) (ReadResult[T], error) {
	s, err := db.read(name, handle)
	if err != nil {
		return ReadResult[T]{}, err
	}
	result, err := f(s)
	if err != nil {
		return ReadResult[T]{}, err
	}
	return ReadResult[T]{s.graph.Name, s.graph.Revision, result}, nil
}

type CatalogResult struct {
	Schema graph.Schema `json:"schema"`
	Summary
}

func (db *Database) Catalog(name, handle string) (ReadResult[CatalogResult], error) {
	return query(db, name, handle, func(s *state) (CatalogResult, error) {
		return CatalogResult{s.graph.Schema, summary(s.graph)}, nil
	})
}

func (db *Database) Vertex(name, handle, id string) (ReadResult[graph.Vertex], error) {
	return query(db, name, handle, func(s *state) (graph.Vertex, error) {
		v, ok := s.graph.Vertices[id]
		if !ok {
			return graph.Vertex{}, graph.Fail(graph.Missing, "vertex %q is not in this revision", id)
		}
		return v, nil
	})
}

type LookupRequest struct {
	Kind     string        `json:"kind"`
	Property string        `json:"property"`
	Value    *graph.Scalar `json:"value,omitempty"`
	Range    *graph.Range  `json:"range,omitempty"`
	After    string        `json:"after,omitempty"`
	Limit    int           `json:"limit,omitempty"`
}

// LookupResult preserves the equality posting fields and adds the ordered-range
// window. Matched is always an exact count over the selected snapshot: for an
// equality it counts equal values, for a range the whole range regardless of
// the returned window.
type LookupResult struct {
	IDs       []string     `json:"ids"`
	Matched   int          `json:"matched"`
	Truncated bool         `json:"truncated"`
	Ordered   []string     `json:"ordered,omitempty"`
	Next      string       `json:"next,omitempty"`
	Range     *graph.Range `json:"range,omitempty"`
}

func (db *Database) Lookup(name, handle string, q LookupRequest) (ReadResult[LookupResult], error) {
	if q.After != "" {
		return db.resumeLookup(name, handle, q)
	}
	return query(db, name, handle, func(s *state) (LookupResult, error) {
		limit, err := walk.Limit(q.Limit)
		if err != nil {
			return LookupResult{}, err
		}
		switch {
		case q.Range != nil && q.Value != nil:
			return LookupResult{}, graph.Fail(graph.Invalid, "lookup takes either value or range, not both")
		case q.Range != nil:
			if _, err := catalog.LookupRangeRule(s.graph.Schema, q.Kind, q.Property, *q.Range); err != nil {
				return LookupResult{}, err
			}
			page, err := s.props.Scan(q.Kind, q.Property, *q.Range, nil, limit)
			if err != nil {
				return LookupResult{}, err
			}
			result := LookupResult{
				IDs: []string{}, Ordered: page.IDs, Matched: page.Matched, Truncated: page.Truncated,
			}
			result.Range = q.Range
			if page.Truncated && page.Last != nil {
				result.Next, err = db.cursors.open(cursor{
					graph: name, view: handle, state: s, kind: q.Kind, property: q.Property,
					bound: *q.Range, pos: *page.Last,
				})
				if err != nil {
					return LookupResult{}, err
				}
			}
			return result, nil
		case q.Value != nil:
			if err := catalog.LookupRule(s.graph.Schema, q.Kind, q.Property, *q.Value); err != nil {
				return LookupResult{}, err
			}
			posting := s.props.Equal(q.Kind, q.Property, *q.Value, limit)
			return LookupResult{IDs: posting.IDs, Matched: posting.Matched, Truncated: posting.Truncated}, nil
		default:
			return LookupResult{}, graph.Fail(graph.Invalid, "lookup requires value, range or after")
		}
	})
}

// resumeLookup reads the next ordered window from the cursor's bound snapshot.
// The head may have advanced since the cursor opened and other commits may be
// in flight; the page still reflects the revision and catalog the cursor was
// issued at. An unknown, released or evicted cursor fails loudly so the client
// restarts rather than stitching revisions.
func (db *Database) resumeLookup(name, handle string, q LookupRequest) (ReadResult[LookupResult], error) {
	// Resolve the selector first so a released pin fails like any other read,
	// then let the cursor enforce that the selector matches the one it bound.
	if _, err := db.read(name, handle); err != nil {
		return ReadResult[LookupResult]{}, err
	}
	limit, err := walk.Limit(q.Limit)
	if err != nil {
		return ReadResult[LookupResult]{}, err
	}
	resumed, err := db.cursors.resume(name, handle, q, limit)
	if err != nil {
		return ReadResult[LookupResult]{}, err
	}
	window := resumed.window
	bound := resumed.bound
	result := LookupResult{
		IDs: []string{}, Ordered: window.IDs, Matched: window.Matched, Truncated: window.Truncated,
		Next: resumed.next, Range: &bound,
	}
	return ReadResult[LookupResult]{name, resumed.revision, result}, nil
}

func (db *Database) Adjacent(name, handle string, q walk.AdjacentRequest) (ReadResult[walk.AdjacentResult], error) {
	return query(db, name, handle, func(s *state) (walk.AdjacentResult, error) {
		return walk.Adjacent(s.graph, s.adj, q)
	})
}

func (db *Database) Reach(name, handle string, q walk.ReachRequest) (ReadResult[walk.ReachResult], error) {
	return query(db, name, handle, func(s *state) (walk.ReachResult, error) {
		return walk.Reach(s.graph, s.adj, q)
	})
}

func (db *Database) Path(name, handle string, q walk.PathRequest) (ReadResult[walk.PathResult], error) {
	return query(db, name, handle, func(s *state) (walk.PathResult, error) {
		return walk.Path(s.graph, s.adj, q)
	})
}

func (db *Database) Components(name, handle string, q walk.ComponentsRequest) (ReadResult[walk.ComponentsResult], error) {
	return query(db, name, handle, func(s *state) (walk.ComponentsResult, error) {
		return walk.Components(s.graph, s.adj, q)
	})
}
