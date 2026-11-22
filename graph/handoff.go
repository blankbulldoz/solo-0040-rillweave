package graph

// Handoff is the provenance manifest attached to a graph derived by the
// handoff workflow. The installable graph state still contains only complete
// vertex records and arcs whose endpoints both exist. Boundary references and
// crossing arcs are documented here instead of weakening that invariant.
type Handoff struct {
	Source     string      `json:"source"`
	Revision   uint64      `json:"revision"`
	Mode       string      `json:"mode"`
	Roots      []string    `json:"roots"`
	Kinds      []string    `json:"kinds"`
	MaxHops    int         `json:"max_hops"`
	Scanned    int         `json:"scanned"`
	Region     []string    `json:"region"`
	References []Reference `json:"references"`
}

// Reference names one original endpoint that stays outside the handed-over
// region. The record carries the endpoint identity only: its kind and
// properties are deliberately not exported. Every original allowed-kind arc
// crossing into it is listed with its own identity and direction, so parallel
// inbound arcs remain distinguishable.
type Reference struct {
	Vertex       string `json:"vertex"`
	IncomingArcs []Arc  `json:"incoming_arcs"`
}
