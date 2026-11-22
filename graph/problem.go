package graph

import "fmt"

type Code string

const (
	Invalid     Code = "invalid"
	Missing     Code = "not_found"
	Conflict    Code = "conflict"
	Budget      Code = "budget_exhausted"
	Unavailable Code = "unavailable"
)

type Problem struct {
	Code    Code   `json:"code"`
	Details string `json:"details"`
	Witness any    `json:"witness,omitempty"`
}

func (p *Problem) Error() string {
	return p.Details
}

func Fail(code Code, format string, args ...any) error {
	return &Problem{Code: code, Details: fmt.Sprintf(format, args...)}
}

// FailWitness attaches machine-readable material to a problem. Callers wrap
// the returned error without losing the witness.
func FailWitness(code Code, witness any, format string, args ...any) error {
	return &Problem{Code: code, Details: fmt.Sprintf(format, args...), Witness: witness}
}
