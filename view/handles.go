package view

import (
	"crypto/rand"
	"encoding/hex"

	"rillweave/graph"
)

const MaxHandles = 16

type held[T any] struct {
	graph string
	value T
}

type Handles[T any] struct {
	live  map[string]held[T]
	limit int
}

func New[T any]() Handles[T] {
	return NewLimited[T](MaxHandles)
}

// NewLimited creates an independent handle pool with its own quota. Pins and
// rehearsal candidates use separate pools so neither consumes the other.
func NewLimited[T any](limit int) Handles[T] {
	return Handles[T]{live: make(map[string]held[T]), limit: limit}
}

func (h Handles[T]) Hold(name string, value T) (string, error) {
	if len(h.live) == h.limit {
		return "", graph.Fail(graph.Budget, "view handle limit reached")
	}
	for {
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return "", graph.Fail(graph.Unavailable, "cannot allocate view handle")
		}
		token := hex.EncodeToString(raw[:])
		if _, exists := h.live[token]; !exists {
			h.live[token] = held[T]{name, value}
			return token, nil
		}
	}
}

// Peek resolves without constructing a failure, so callers can distinguish an
// absent handle in this pool from one held by a different pool.
func (h Handles[T]) Peek(name, token string) (T, bool) {
	record, exists := h.live[token]
	if !exists || record.graph != name {
		var zero T
		return zero, false
	}
	return record.value, true
}

func (h Handles[T]) Resolve(name, token string) (T, error) {
	if value, ok := h.Peek(name, token); ok {
		return value, nil
	}
	var zero T
	return zero, graph.Fail(graph.Missing, "view handle is not available for this graph")
}

func (h Handles[T]) Release(name, token string) error {
	if _, err := h.Resolve(name, token); err != nil {
		return err
	}
	delete(h.live, token)
	return nil
}
