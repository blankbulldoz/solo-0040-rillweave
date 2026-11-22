package engine

import (
	"fmt"

	"rillweave/catalog"
	"rillweave/graph"
	"rillweave/index"
	"rillweave/walk"
)

type ReadResult[T any] struct {
	Graph    string `json:"graph"`
	Revision uint64 `json:"revision"`
	Result   T      `json:"result"`
	// Validator is the response credential used for conditional revalidation.
	// It travels in the ETag header and is never serialized into the body.
	Validator string `json:"-"`
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
	return ReadResult[T]{Graph: s.graph.Name, Revision: s.graph.Revision, Result: result}, nil
}

// validator stamps a read body with the exact state it was drawn from: the
// durable graph identity, the selected revision and the resource
// discriminator. Any commit bumps the revision; installing a graph (creation,
// restore, or a different graph reopened under the same name) changes the
// identity, so a credential never outlives the state it describes.
func validator(g *graph.Graph, resource string) string {
	return fmt.Sprintf("v1:%s:%d:%s", g.Identity, g.Revision, resource)
}

// conditional runs a read whose response the caller may cache and revalidate
// against a later request; see validator for what the credential covers.
func conditional[T any](db *Database, name, handle, resource string, f func(*state) (T, error)) (ReadResult[T], error) {
	s, err := db.read(name, handle)
	if err != nil {
		return ReadResult[T]{}, err
	}
	result, err := f(s)
	if err != nil {
		return ReadResult[T]{}, err
	}
	out := ReadResult[T]{Graph: s.graph.Name, Revision: s.graph.Revision, Result: result}
	out.Validator = validator(s.graph, resource)
	return out, nil
}

type CatalogResult struct {
	Schema graph.Schema `json:"schema"`
	Summary
}

func (db *Database) Catalog(name, handle string) (ReadResult[CatalogResult], error) {
	return conditional(db, name, handle, "catalog", func(s *state) (CatalogResult, error) {
		return CatalogResult{s.graph.Schema, summary(s.graph)}, nil
	})
}

func (db *Database) Vertex(name, handle, id string) (ReadResult[graph.Vertex], error) {
	return conditional(db, name, handle, "vertex:"+id, func(s *state) (graph.Vertex, error) {
		v, ok := s.graph.Vertices[id]
		if !ok {
			return graph.Vertex{}, graph.Fail(graph.Missing, "vertex %q is not in this revision", id)
		}
		return v, nil
	})
}

type LookupRequest struct {
	Kind     string       `json:"kind"`
	Property string       `json:"property"`
	Value    graph.Scalar `json:"value"`
	Limit    int          `json:"limit,omitempty"`
}

func (db *Database) Lookup(name, handle string, q LookupRequest) (ReadResult[index.Posting], error) {
	return query(db, name, handle, func(s *state) (index.Posting, error) {
		if err := catalog.LookupRule(s.graph.Schema, q.Kind, q.Property, q.Value); err != nil {
			return index.Posting{}, err
		}
		limit, err := walk.Limit(q.Limit)
		if err != nil {
			return index.Posting{}, err
		}
		return s.props.Equal(q.Kind, q.Property, q.Value, limit), nil
	})
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
