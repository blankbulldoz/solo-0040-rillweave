package engine

import (
	"rillweave/change"
	"rillweave/graph"
	"rillweave/image"
)

func (db *Database) resolve(name, handle string) (*state, error) {
	if err := db.available(); err != nil {
		return nil, err
	}
	if handle != "" {
		return db.held.Resolve(name, handle)
	}
	if head := db.heads[name]; head != nil {
		return head, nil
	}
	return nil, graph.Fail(graph.Missing, "graph %q not found", name)
}

func (db *Database) read(name, handle string) (*state, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return db.resolve(name, handle)
}

type PinResult struct {
	Handle string `json:"handle"`
	Summary
}

func (db *Database) Hold(name string) (PinResult, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	head, err := db.resolve(name, "")
	if err != nil {
		return PinResult{}, err
	}
	token, err := db.held.Hold(name, head)
	if err != nil {
		return PinResult{}, err
	}
	return PinResult{token, summary(head.graph)}, nil
}

func (db *Database) Release(name, handle string) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.available(); err != nil {
		return err
	}
	return db.held.Release(name, handle)
}

func (db *Database) Comparison(name, fromHandle, toHandle string) (change.Difference, error) {
	from, to, err := db.pair(name, fromHandle, toHandle)
	if err != nil {
		return change.Difference{}, err
	}
	return change.Compare(from.graph, to.graph), nil
}

type ContractRequest struct {
	From         string                `json:"from,omitempty"`
	To           string                `json:"to,omitempty"`
	WitnessLimit int                   `json:"witness_limit,omitempty"`
	Contracts    []change.ReadContract `json:"contracts"`
}

// ContractComparison resolves both pins before auditing any registered read
// condition, so every conclusion carries the two revisions it was drawn from.
func (db *Database) ContractComparison(name string, q ContractRequest) (change.ContractComparison, error) {
	from, to, err := db.pair(name, q.From, q.To)
	if err != nil {
		return change.ContractComparison{}, err
	}
	result, err := change.CompareContracts(from.graph, to.graph, q.Contracts, q.WitnessLimit)
	if err != nil {
		return change.ContractComparison{}, err
	}
	result.FromHandle, result.ToHandle = q.From, q.To
	return result, nil
}

// ComparisonWithContracts keeps the identity difference of Comparison and adds
// the registered read-contract audit. Without contracts it is exactly
// Comparison, so existing compare requests and responses are unchanged.
func (db *Database) ComparisonWithContracts(name string, q ContractRequest) (change.ComparisonAudit, error) {
	if q.Contracts == nil {
		difference, err := db.Comparison(name, q.From, q.To)
		return change.ComparisonAudit{Difference: difference}, err
	}
	from, to, err := db.pair(name, q.From, q.To)
	if err != nil {
		return change.ComparisonAudit{}, err
	}
	contracts, err := change.CompareContracts(from.graph, to.graph, q.Contracts, q.WitnessLimit)
	if err != nil {
		return change.ComparisonAudit{}, err
	}
	contracts.FromHandle, contracts.ToHandle = q.From, q.To
	return change.ComparisonAudit{
		Difference:    change.Compare(from.graph, to.graph),
		ContractAudit: &contracts,
	}, nil
}

func (db *Database) pair(name, fromHandle, toHandle string) (*state, *state, error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	from, err := db.resolve(name, fromHandle)
	if err != nil {
		return nil, nil, err
	}
	to, err := db.resolve(name, toHandle)
	return from, to, err
}

func (db *Database) Export(name, handle string) (image.Envelope, error) {
	s, err := db.read(name, handle)
	if err != nil {
		return image.Envelope{}, err
	}
	return image.Encode(s.graph)
}
