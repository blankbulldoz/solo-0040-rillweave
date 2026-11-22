package walk

import (
	"slices"
	"strconv"
	"strings"

	"rillweave/graph"
)

const (
	maxPatternNodes  = 64
	maxPatternDepth  = 16
	maxProgramStates = 256
)

// Pattern is a regular expression over arc kinds. Exactly one field is set
// per node: kinds matches a single arc, seq and alt combine members in order
// or by choice, and star, plus or opt repeat their member. Kind membership is
// judged against the arc catalog of the selected graph revision.
type Pattern struct {
	Kinds []string  `json:"kinds,omitempty"`
	Seq   []Pattern `json:"seq,omitempty"`
	Alt   []Pattern `json:"alt,omitempty"`
	Star  *Pattern  `json:"star,omitempty"`
	Plus  *Pattern  `json:"plus,omitempty"`
	Opt   *Pattern  `json:"opt,omitempty"`
}

func (p Pattern) validate(schema graph.Schema, depth int, nodes *int) error {
	*nodes++
	switch {
	case *nodes > maxPatternNodes:
		return graph.Fail(graph.Invalid, "pattern exceeds %d nodes", maxPatternNodes)
	case depth > maxPatternDepth:
		return graph.Fail(graph.Invalid, "pattern nests deeper than %d", maxPatternDepth)
	}
	set := 0
	for _, present := range []bool{
		p.Kinds != nil, p.Seq != nil, p.Alt != nil,
		p.Star != nil, p.Plus != nil, p.Opt != nil,
	} {
		if present {
			set++
		}
	}
	if set != 1 {
		return graph.Fail(graph.Invalid, "pattern node must set exactly one of kinds, seq, alt, star, plus or opt")
	}
	children := []Pattern{}
	switch {
	case p.Kinds != nil:
		if len(p.Kinds) == 0 || len(p.Kinds) > graph.MaxRules {
			return graph.Fail(graph.Invalid, "kinds leaf requires 1..%d arc kinds", graph.MaxRules)
		}
		for _, kind := range p.Kinds {
			if _, ok := schema.Arcs[kind]; !ok {
				return graph.Fail(graph.Invalid, "arc kind %q is not in this revision's catalog", kind)
			}
		}
	case p.Seq != nil:
		children = p.Seq
	case p.Alt != nil:
		children = p.Alt
	case p.Star != nil:
		children = []Pattern{*p.Star}
	case p.Plus != nil:
		children = []Pattern{*p.Plus}
	case p.Opt != nil:
		children = []Pattern{*p.Opt}
	}
	if p.Kinds == nil && len(children) == 0 {
		return graph.Fail(graph.Invalid, "pattern combinator requires at least one member")
	}
	for i := range children {
		if err := children[i].validate(schema, depth+1, nodes); err != nil {
			return err
		}
	}
	return nil
}

// nfaEdge with nil kinds is an epsilon move.
type nfaEdge struct {
	to    int
	kinds []string
}

type compiler struct {
	edges [][]nfaEdge
}

func (c *compiler) state() int {
	c.edges = append(c.edges, nil)
	return len(c.edges) - 1
}

func (c *compiler) move(from, to int, kinds []string) {
	c.edges[from] = append(c.edges[from], nfaEdge{to, kinds})
}

func (c *compiler) compile(p Pattern) (int, int) {
	start, accept := c.state(), c.state()
	switch {
	case p.Kinds != nil:
		c.move(start, accept, p.Kinds)
	case p.Seq != nil:
		tail := start
		for _, child := range p.Seq {
			from, to := c.compile(child)
			c.move(tail, from, nil)
			tail = to
		}
		c.move(tail, accept, nil)
	case p.Alt != nil:
		for _, child := range p.Alt {
			from, to := c.compile(child)
			c.move(start, from, nil)
			c.move(to, accept, nil)
		}
	case p.Star != nil:
		from, to := c.compile(*p.Star)
		c.move(start, accept, nil)
		c.move(start, from, nil)
		c.move(to, from, nil)
		c.move(to, accept, nil)
	case p.Plus != nil:
		from, to := c.compile(*p.Plus)
		c.move(start, from, nil)
		c.move(to, from, nil)
		c.move(to, accept, nil)
	case p.Opt != nil:
		from, to := c.compile(*p.Opt)
		c.move(start, accept, nil)
		c.move(start, from, nil)
		c.move(to, accept, nil)
	}
	return start, accept
}

// program is the deterministic form of a pattern: one start state, per-state
// kind transitions and an accepting flag per state.
type program struct {
	start  int
	accept []bool
	trans  []map[string]int
}

func (p *program) step(state int, kind string) (int, bool) {
	next, ok := p.trans[state][kind]
	return next, ok
}

func compilePattern(schema graph.Schema, pattern Pattern) (*program, error) {
	nodes := 0
	if err := pattern.validate(schema, 0, &nodes); err != nil {
		return nil, err
	}
	c := &compiler{}
	start, accept := c.compile(pattern)
	alphabet := map[string]bool{}
	for _, edges := range c.edges {
		for _, edge := range edges {
			for _, kind := range edge.kinds {
				alphabet[kind] = true
			}
		}
	}
	kinds := make([]string, 0, len(alphabet))
	for kind := range alphabet {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	seed := closeOver(c.edges, []int{start})
	states := [][]int{seed}
	ids := map[string]int{stateKey(seed): 0}
	prog := &program{start: 0}
	for i := 0; i < len(states); i++ {
		prog.trans = append(prog.trans, map[string]int{})
		prog.accept = append(prog.accept, slices.Contains(states[i], accept))
		for _, kind := range kinds {
			var reach []int
			for _, from := range states[i] {
				for _, edge := range c.edges[from] {
					if slices.Contains(edge.kinds, kind) {
						reach = append(reach, edge.to)
					}
				}
			}
			if len(reach) == 0 {
				continue
			}
			next := closeOver(c.edges, reach)
			id, ok := ids[stateKey(next)]
			if !ok {
				if len(states) == maxProgramStates {
					return nil, graph.Fail(graph.Invalid, "pattern exceeds %d deterministic stages", maxProgramStates)
				}
				id = len(states)
				ids[stateKey(next)] = id
				states = append(states, next)
			}
			prog.trans[i][kind] = id
		}
	}
	return prog, nil
}

func closeOver(edges [][]nfaEdge, seed []int) []int {
	seen := make(map[int]bool, len(seed))
	stack := slices.Clone(seed)
	for _, from := range seed {
		seen[from] = true
	}
	for len(stack) != 0 {
		from := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, edge := range edges[from] {
			if edge.kinds == nil && !seen[edge.to] {
				seen[edge.to] = true
				stack = append(stack, edge.to)
			}
		}
	}
	set := make([]int, 0, len(seen))
	for from := range seen {
		set = append(set, from)
	}
	slices.Sort(set)
	return set
}

func stateKey(set []int) string {
	parts := make([]string, len(set))
	for i, from := range set {
		parts[i] = strconv.Itoa(from)
	}
	return strings.Join(parts, ",")
}
