package graph

import "fmt"

type Code string

const (
	Invalid     Code = "invalid"
	Missing     Code = "not_found"
	Conflict    Code = "conflict"
	Budget      Code = "budget_exhausted"
	Unavailable Code = "unavailable"
	// RebaseConflict marks an opt-in rebase whose intervening head changes make
	// an automatic replay unsafe; the caller must reconcile the batch manually.
	RebaseConflict Code = "rebase_conflict"
	// RebaseUnavailable marks an opt-in rebase whose base revision is no longer
	// held in the bounded process-local rebase window, so safety is undecidable.
	RebaseUnavailable Code = "rebase_unavailable"
)

type Problem struct {
	Code    Code   `json:"code"`
	Details string `json:"details"`
}

func (p *Problem) Error() string {
	return p.Details
}

func Fail(code Code, format string, args ...any) error {
	return &Problem{Code: code, Details: fmt.Sprintf(format, args...)}
}
