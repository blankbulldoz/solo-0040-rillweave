package engine

import (
	"rillweave/change"
	"rillweave/graph"
)

// PurgePreview plans a batch vertex removal against the selected view. The
// plan is pure computation; nothing is staged or published.
func (db *Database) PurgePreview(name, handle string, q change.Request) (ReadResult[change.Plan], error) {
	return query(db, name, handle, func(s *state) (change.Plan, error) {
		return change.PlanPurge(s.graph, s.adj, q)
	})
}

// Purge confirms a previewed batch removal. The base revision must still be
// the head, the confirmed arcs must equal the removal closure and every
// preserved pair must keep a route; a rejected purge changes nothing.
func (db *Database) Purge(name string, q change.Confirmation) (Summary, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	head, err := db.resolve(name, "")
	if err != nil {
		return Summary{}, err
	}
	if q.Base != head.graph.Revision {
		return Summary{}, graph.Fail(graph.Conflict, "base %d is not current revision %d", q.Base, head.graph.Revision)
	}
	plan, err := change.PlanPurge(head.graph, head.adj, q.Request)
	if err != nil {
		return Summary{}, err
	}
	if err := plan.ConfirmArcs(q.Arcs); err != nil {
		return Summary{}, err
	}
	if plan.Blocked {
		for _, pair := range plan.Pairs {
			if pair.Status == change.Broken {
				return Summary{}, graph.Fail(graph.Conflict,
					"purge blocked: preserved pair %q -> %q loses all routes", pair.From, pair.To)
			}
		}
	}
	next, err := change.ApplyPurge(head.graph, plan)
	if err != nil {
		return Summary{}, err
	}
	return db.publish(next)
}
