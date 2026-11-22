# Rillweave

Rillweave is a persistent property graph database exposed by an HTTP JSON
service. Applications store directed typed dependencies and ask structural
questions against a single graph revision. Each namespace has its own schema,
vertex keys, arc keys and monotonic revision. Parallel arcs and self-loops
retain individual identities. Graph `abutment` and vertices `hinge`, `keel`
and `spar` in the examples are synthetic.

## Ownership

| Package | Responsibility |
| --- | --- |
| graph | Typed scalar values, identifiers, vertex and arc documents |
| catalog | Rule definitions, endpoint compatibility and final-state uniqueness |
| change | Private staged edit application and logical difference |
| index | Deterministic adjacency and typed vertex postings |
| walk | Graph query semantics, expansion budgets, witnesses, component membership and cross-group flow |
| image | Portable versioned encoding, size limit and integrity |
| disk | Exclusive writer ownership and durable file replacement |
| view | Bounded handles to immutable read states |
| engine | Head publication, pinned access and restore semantics |
| wire | HTTP requests, bounded decoding and domain error mapping |

## Rules and lifecycles

Graph revisions start at 1. A commit includes the expected head revision and
a nonempty ordered edit set. Individual edits operate on a private candidate;
its final schema, vertices, arcs and unique values must agree before any
publication. Replacing both ends of a unique value swap is allowed. Dropping a
vertex while leaving any adjacent arc is rejected. Positive arc costs are
integers. Properties are strings, signed 64-bit integers or booleans;
undeclared properties, explicit nulls and type coercion are not supported.

The graph namespace moves from absent to installed. An edit set moves from
candidate to validated to published or rejected. A graph revision can move
from head to detached; explicitly held references remain usable. A handle
moves from held to released (or disappears on restart). An image moves from
encoded to verified to installed; malformed images never become a graph.
Secondary indexes are rebuilt from the accepted immutable state.

Publishing a head writes and synchronizes a temporary image, replaces the
node's durable graph file and synchronizes the directory. A failure after the
rename but before confirmed durability makes further graph operations fail
until restart. The instance must own its data directory exclusively.

## Public Workflows

| Workflow | Success | Alternative or rejection |
| --- | --- | --- |
| catalog | Install or atomically replace typed rules; inspect a chosen view | Rule changes leaving incompatible graph state are rejected |
| commit | Publish a final consistent graph at the next revision | Stale or invalid edits do not publish; unique swaps may apply in one revision |
| vertex | Return exact contents and chosen revision | A vertex missing from that view returns not found |
| lookup | Resolve typed equality via a vertex posting | Missing properties do not match a present value; wrong types are rejected |
| adjacency | List directed arc identities and endpoints | Both directions include a self-loop once; mismatched kinds do not pass |
| reach | Return minimum hop distances, including the selected root | Insufficient expansion budget returns an error, never a complete-looking prefix |
| path | Return least total cost within the hop limit and the witnessing arcs | No route within the chosen subgraph is distinct from budget exhaustion |
| components | Return strongly connected components and directed links between groups | Weak connectivity and parallel arcs do not merge or multiply component links |
| flow | Estimate independent cross-group throughput from an entry group to an exit group under an optional caller limit | Same-group endpoints are invalid; an exhausted budget or a disconnected pair is distinct from a bounded sufficient answer |
| pin | Hold and query a fixed revision, then release it | A handle from another graph, an exhausted quota or a released handle is rejected |
| compare | Compare two views of the same graph by entity identity and schema change | A foreign handle cannot participate |
| backup | Export one selected revision and import it under an unused name | Malformed images and occupied names do not overwrite existing state |
| restart | Reopen durable graphs and reconstruct indexes | Full corruption fails startup; ephemeral view handles are not recovered |

The graph, revision, traversal constraints and work budget are visible through
the protocol documented in README. Validate by building and running each
bounded walkthrough against a temporary database. Formal tests are deferred.

## Cross-group flow

The flow query reuses the strongly connected partition of a components query
over the same selected relations. Membership in one group is a stated business
convention: any member can hand work to any other member without consuming
capacity, so groups become the nodes of a directed quotient. Every original
arc whose endpoints land in different groups becomes one independent quotient
edge whose positive integer capacity is the arc's stored cost; parallel arcs
stay independent and add. A request names entry and exit groups by any member
vertex; two members of one group cannot be a flow pair.

A positive caller limit bounds the search. Reaching it returns a sufficient
answer at exactly the limit without proving the full maximum or any cut;
otherwise the exact maximum is returned together with a minimum directed cut
expressed as the original bottleneck arcs. Every result also lists per-arc
assigned flow and a deterministic route decomposition whose bundles sum to the
reported amount, each route naming its group sequence and witnessing arcs.
Augmentation moves capacity in whole-arc bulk rather than one unit per step, so
wide arcs beside small limits do not expand per unit. No route is a valid
`found:false` result, separate from invalid endpoints and budget exhaustion.

## Boundaries

The engine is designed for bounded local graphs. It clones the candidate and
rebuilds indexes per commit. It does not claim distributed replication, online
index building, arbitrary query languages, authentication, multi-graph atomic
commits or durable revision history. The image digest establishes byte
integrity, not authenticity. Run the service only in a trusted environment.
