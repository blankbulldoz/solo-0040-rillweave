package assemble

import (
	"fmt"

	"rillweave/graph"
	"rillweave/ingest"
	"rillweave/profile"
)

// StarterDecisions turns each finding's first applicable plan into an
// editable decisions document. Blockers that cannot be repaired by a
// naming action use the plan that keeps the most data (for example
// dropping one unsupported property rather than the whole record).
// The owner edits this file; it is never regenerated over an edit.
func StarterDecisions(p *profile.Profile, graphName string) *Decisions {
	d := &Decisions{
		Version:       1,
		ProfileDigest: p.Digest,
		GraphName:     graphName,
		Actions:       []Action{},
		Notes: []string{
			"Generated from the profile. Review every action and remove or replace any you do not confirm.",
			"explicit_cast actions require an owner_note and are never emitted by default.",
		},
	}
	seen := map[string]bool{}
	add := func(acts ...Action) {
		for _, act := range acts {
			key := act.Op + "|" + act.ScopeRef + "|" + act.EntityKind + "|" + act.ArcKind +
				"|" + act.Property + "|" + act.ObservedKind + "|" + act.Suffix + "|" +
				act.NewName + "|" + act.From + "|" + act.To
			if seen[key] {
				continue
			}
			seen[key] = true
			d.Actions = append(d.Actions, act)
		}
	}
	for _, finding := range p.Findings {
		if len(finding.Plans) == 0 {
			continue
		}
		plan := finding.Plans[0]
		for _, pa := range plan.Actions {
			add(fromPlanAction(pa))
		}
	}
	return d
}

func fromPlanAction(pa profile.PlanAction) Action {
	return Action{
		Op:           pa.Op,
		EntityKind:   pa.EntityKind,
		ArcKind:      pa.ArcKind,
		Property:     pa.Property,
		ObservedKind: pa.ObservedKind,
		Suffix:       pa.Suffix,
		NewName:      pa.NewName,
		From:         pa.From,
		To:           pa.To,
		ScopeRef:     pa.ScopeRef,
	}
}

// VerifyInputs re-checks the input checksums embedded in the profile and
// fails fast if any file changed since inference. This is the guarantee
// that naming iterations never run against mutated source data.
func VerifyInputs(p *profile.Profile, files []ingest.InputFile) error {
	if len(files) != len(p.InputDigest) {
		return fmt.Errorf("input set changed: profile recorded %d files, build sees %d",
			len(p.InputDigest), len(files))
	}
	for i, want := range p.InputDigest {
		got := files[i]
		if got.Path != want.Path || got.SHA256 != want.SHA256 {
			return fmt.Errorf("input %q changed since inference (profile %s, current %s); rerun infer",
				want.Path, want.SHA256, got.SHA256)
		}
	}
	return nil
}

// CheckProfile reports whether the profile still contains unresolved
// blockers or conflicts. The CLI uses it for the `check` subcommand.
func CheckProfile(p *profile.Profile) (blockers, conflicts int, advisory int) {
	for _, f := range p.Findings {
		switch f.Severity {
		case profile.SeverityBlocker:
			blockers++
		case profile.SeverityConflict:
			conflicts++
		default:
			advisory++
		}
	}
	return
}

var _ = graph.Text
