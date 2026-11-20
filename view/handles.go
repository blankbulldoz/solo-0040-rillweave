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
	live map[string]held[T]
}

func New[T any]() Handles[T] {
	return Handles[T]{live: make(map[string]held[T])}
}

func (h Handles[T]) Hold(name string, value T) (string, error) {
	if len(h.live) == MaxHandles {
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

func (h Handles[T]) Resolve(name, token string) (T, error) {
	if record, exists := h.live[token]; exists && record.graph == name {
		return record.value, nil
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
