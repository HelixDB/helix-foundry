// Package automation is what a Foundry automation is: a graph a person draws
// on a canvas, read from its one trigger along its edges, of steps run as the
// automation's owner (its spec); when its schedule fires next (Schedule);
// which step of a run comes next, given what each step did (Spec.Next); how a
// condition picks its branch (Check); and which of the Assistant's actions an
// owner approved for a step (Action.Fits).
//
// An automation is WHEN <trigger> THEN <steps>: an Assistant step runs an
// instruction as an Assistant turn as the owner, an action step runs one
// allowlisted operation with a payload the owner approved, a condition picks
// one of its two branches from the step before it, and a notify step emails
// the owner. "Workflows" is another thing: an outside agent's recorded runs.
//
// Nothing here talks to a store, a model or the network: the API, the MCP's
// operations, the Assistant and the foundry-automation-worker all read and
// check specs through it.
package automation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SpecVersion is the version of the spec this package reads and writes.
const SpecVersion = 1

const (
	// MaxNodes is the most nodes a spec holds, its trigger included.
	MaxNodes = 50
	// MaxEdges is the most edges a spec holds.
	MaxEdges = 100
	// MaxInstructionChars bounds an Assistant step's instruction.
	MaxInstructionChars = 2000
	// MaxMessageChars bounds a notify step's message.
	MaxMessageChars = 500
	// MaxAllowed is the most actions an Assistant step may run unasked.
	MaxAllowed = 10
	// MaxSpecBytes bounds a spec as a caller sends it.
	MaxSpecBytes = 64 << 10
	// maxPosition bounds a node's position on the canvas, either way.
	maxPosition = 1e6
)

var (
	// nodeIDPattern is a node's id: short, lowercase, unique in its spec.
	nodeIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
	// idPattern is a connector's, an ontology's, a set's or a dashboard's id.
	idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

// Spec is an automation's graph: exactly one trigger, every other node
// reachable from it along edges that make no cycle. Only ParseSpec makes
// one, so every Spec holds.
type Spec struct {
	nodes []Node
	edges []Edge
	// order is every node's id in a topological order: the trigger first,
	// and among nodes ready together, the order they were given in.
	order []string
}

// Node is one step of the graph. Its position is where the canvas draws it,
// and never changes what runs.
type Node struct {
	ID       string
	Config   Config
	Position Position
}

// Position is a node's place on the canvas.
type Position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// Config is what a node does: a closed set.
type Config interface{ kind() NodeKind }

// NodeKind names a node's kind as the spec writes it.
type NodeKind string

const (
	KindTrigger   NodeKind = "trigger"
	KindAssistant NodeKind = "assistant"
	KindAction    NodeKind = "action"
	KindCondition NodeKind = "condition"
	KindNotify    NodeKind = "notify"
)

// TriggerNode starts every run: the spec's one node with no edge in.
type TriggerNode struct{ Trigger Trigger }

// AssistantNode runs Instruction as an Assistant turn as the owner. Allow is
// what the owner approved it to do unasked: an action it prepares that one of
// them fits (Action.Fits) runs at once; any other waits for the owner as a
// card.
type AssistantNode struct {
	Instruction string
	Allow       []Action
}

// ActionNode runs one action, as the owner approved it.
type ActionNode struct{ Action Action }

// ConditionNode takes its true or its false branch by Check of the one step
// before it.
type ConditionNode struct{ Check Check }

// NotifyNode emails the owner Message, with what the run's Assistant steps
// before it said.
type NotifyNode struct{ Message string }

func (TriggerNode) kind() NodeKind   { return KindTrigger }
func (AssistantNode) kind() NodeKind { return KindAssistant }
func (ActionNode) kind() NodeKind    { return KindAction }
func (ConditionNode) kind() NodeKind { return KindCondition }
func (NotifyNode) kind() NodeKind    { return KindNotify }

// Kind is the node's kind.
func (n Node) Kind() NodeKind { return n.Config.kind() }

// Branch is which way an edge out of a condition goes: a closed set.
// BranchNone is every other edge's.
type Branch string

const (
	BranchNone  Branch = ""
	BranchTrue  Branch = "true"
	BranchFalse Branch = "false"
)

// Edge goes from one node to the next; Branch is set exactly on an edge out
// of a condition.
type Edge struct {
	From, To string
	Branch   Branch
}

// Nodes are the spec's nodes, in the order they were given.
func (s Spec) Nodes() []Node { return slices.Clone(s.nodes) }

// Edges are the spec's edges, in the order they were given.
func (s Spec) Edges() []Edge { return slices.Clone(s.edges) }

// Order is every node's id, the trigger first, each after every node with an
// edge into it.
func (s Spec) Order() []string { return slices.Clone(s.order) }

// Node is the node of id; false for none.
func (s Spec) Node(id string) (Node, bool) {
	i := slices.IndexFunc(s.nodes, func(n Node) bool { return n.ID == id })
	if i < 0 {
		return Node{}, false
	}
	return s.nodes[i], true
}

// Trigger is what starts the automation's runs.
func (s Spec) Trigger() Trigger {
	for _, n := range s.nodes {
		if t, ok := n.Config.(TriggerNode); ok {
			return t.Trigger
		}
	}
	panic("automation: a parsed spec has its trigger")
}

// TriggerID is the trigger node's id.
func (s Spec) TriggerID() string { return s.order[0] }

// In are the edges into id, in the order they were given.
func (s Spec) In(id string) []Edge {
	return slices.DeleteFunc(s.Edges(), func(e Edge) bool { return e.To != id })
}

// Ancestors are the ids of the nodes id is reachable from, in Order.
func (s Spec) Ancestors(id string) []string {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(to string) {
		for _, e := range s.In(to) {
			if !seen[e.From] {
				seen[e.From] = true
				walk(e.From)
			}
		}
	}
	walk(id)
	return slices.DeleteFunc(s.Order(), func(n string) bool { return !seen[n] })
}

// Targets are the ids the spec acts on or listens to, by kind, each once, for
// the API to check they are the workspace's.
type Targets struct {
	Connectors, Ontologies, Sets, Dashboards []string
}

// Targets names what the spec's trigger listens to and its actions act on.
func (s Spec) Targets() Targets {
	var t Targets
	add := func(list *[]string, id string) {
		if !slices.Contains(*list, id) {
			*list = append(*list, id)
		}
	}
	addAction := func(a Action) {
		switch a := a.(type) {
		case SyncConnector:
			add(&t.Connectors, a.ConnectorID)
		case CreateLoad:
			add(&t.Ontologies, a.OntologyID)
		case PublishDashboard:
			add(&t.Dashboards, a.DashboardID)
		}
	}
	for _, n := range s.nodes {
		switch c := n.Config.(type) {
		case TriggerNode:
			switch tr := c.Trigger.(type) {
			case SyncFinished:
				add(&t.Connectors, tr.ConnectorID)
			case LoadReady:
				add(&t.Ontologies, tr.OntologyID)
			case DocumentArrived:
				add(&t.Sets, tr.SetID)
			case GenerationFinished:
				add(&t.Ontologies, tr.OntologyID)
			}
		case AssistantNode:
			for _, a := range c.Allow {
				addAction(a)
			}
		case ActionNode:
			addAction(c.Action)
		}
	}
	return t
}

// wireSpec is a spec as JSON:
//
//	{"v": 1,
//	 "nodes": [{"id": "start", "kind": "trigger", "config": {"kind": "schedule", "every": "week", "weekday": "monday", "at": "09:00", "time_zone": "Europe/London"}, "position": {"x": 0, "y": 0}},
//	           {"id": "sync", "kind": "action", "config": {"operation": "sync_connector", "connector_id": "…"}, "position": {"x": 240, "y": 0}}],
//	 "edges": [{"from": "start", "to": "sync"}]}
type wireSpec struct {
	V     int        `json:"v"`
	Nodes []wireNode `json:"nodes"`
	Edges []wireEdge `json:"edges"`
}

type wireNode struct {
	ID       string          `json:"id"`
	Kind     NodeKind        `json:"kind"`
	Config   json.RawMessage `json:"config"`
	Position Position        `json:"position"`
}

type wireEdge struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Branch Branch `json:"branch,omitempty"`
}

// ParseSpec reads a spec from JSON, refusing anything but the shape above: an
// unknown field, a version other than SpecVersion, no node or more than
// MaxNodes, more than MaxEdges edges, a node id that isn't unique or doesn't
// fit, a config that isn't its kind's, a position off the canvas, and any
// graph but one trigger with no edge in, from which every node is reachable
// along edges that make no cycle; a branch exactly on each edge out of a
// condition, each branch at most once; a condition with exactly one edge in,
// from a step whose output it can check; at least one step after the
// trigger. Its errors say what is wrong for whoever drew the graph: a person
// or an agent.
//
//	spec, err := automation.ParseSpec([]byte(raw))
func ParseSpec(raw []byte) (Spec, error) {
	if len(raw) > MaxSpecBytes {
		return Spec{}, fmt.Errorf("a spec is at most %d KiB", MaxSpecBytes>>10)
	}
	var w wireSpec
	if err := decodeStrict(raw, &w); err != nil {
		return Spec{}, fmt.Errorf("the spec is not {v, nodes, edges}: %w", err)
	}
	switch {
	case w.V != SpecVersion:
		return Spec{}, fmt.Errorf("the spec's v is %d; this Foundry reads v %d", w.V, SpecVersion)
	case len(w.Nodes) < 2 || len(w.Nodes) > MaxNodes:
		return Spec{}, fmt.Errorf("an automation has a trigger and at least one step, at most %d nodes in all, not %d", MaxNodes, len(w.Nodes))
	case len(w.Edges) > MaxEdges:
		return Spec{}, fmt.Errorf("an automation has at most %d edges, not %d", MaxEdges, len(w.Edges))
	}
	s := Spec{}
	for _, wn := range w.Nodes {
		n, err := parseNode(wn)
		if err != nil {
			return Spec{}, err
		}
		if _, dup := s.Node(n.ID); dup {
			return Spec{}, fmt.Errorf("two nodes have the id %s", n.ID)
		}
		s.nodes = append(s.nodes, n)
	}
	for _, we := range w.Edges {
		s.edges = append(s.edges, Edge(we))
	}
	if err := s.check(); err != nil {
		return Spec{}, err
	}
	return s, nil
}

// check holds the graph's rules, and orders it.
func (s *Spec) check() error {
	triggers := slices.DeleteFunc(s.Nodes(), func(n Node) bool { return n.Kind() != KindTrigger })
	if len(triggers) != 1 {
		return fmt.Errorf("an automation has exactly one trigger, not %d", len(triggers))
	}
	seen := map[[2]string]bool{}
	for _, e := range s.edges {
		from, ok := s.Node(e.From)
		if !ok {
			return fmt.Errorf("an edge comes from %q, which is no node", e.From)
		}
		to, ok := s.Node(e.To)
		switch {
		case !ok:
			return fmt.Errorf("an edge goes to %q, which is no node", e.To)
		case e.From == e.To:
			return fmt.Errorf("node %s has an edge to itself", e.From)
		case to.Kind() == KindTrigger:
			return fmt.Errorf("the trigger %s starts the automation: no edge goes into it", e.To)
		case seen[[2]string{e.From, e.To}]:
			return fmt.Errorf("two edges go from %s to %s", e.From, e.To)
		case from.Kind() == KindCondition && e.Branch != BranchTrue && e.Branch != BranchFalse:
			return fmt.Errorf("the edge from condition %s to %s is its true or its false branch", e.From, e.To)
		case from.Kind() != KindCondition && e.Branch != BranchNone:
			return fmt.Errorf("the edge from %s to %s has a branch, which only a condition's edges have", e.From, e.To)
		}
		seen[[2]string{e.From, e.To}] = true
	}
	for _, n := range s.nodes {
		if n.Kind() != KindCondition {
			continue
		}
		for _, b := range []Branch{BranchTrue, BranchFalse} {
			if count := len(slices.DeleteFunc(s.Edges(), func(e Edge) bool { return e.From != n.ID || e.Branch != b })); count > 1 {
				return fmt.Errorf("condition %s has %d %s branches; it has at most one", n.ID, count, b)
			}
		}
		in := s.In(n.ID)
		if len(in) != 1 {
			return fmt.Errorf("condition %s checks the one step before it, so exactly one edge goes into it, not %d", n.ID, len(in))
		}
		from, _ := s.Node(in[0].From)
		if !n.Config.(ConditionNode).Check.checks(from.Config) {
			return fmt.Errorf("condition %s can't check %s: %s", n.ID, from.ID, n.Config.(ConditionNode).Check.needs())
		}
	}
	order, err := s.topological(triggers[0].ID)
	if err != nil {
		return err
	}
	s.order = order
	return nil
}

// topological orders the nodes from trigger (Kahn's, ties in the order given),
// refusing a cycle or a node the trigger doesn't reach.
func (s Spec) topological(trigger string) ([]string, error) {
	in := map[string]int{}
	for _, e := range s.edges {
		in[e.To]++
	}
	ready := []string{trigger}
	var order []string
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for _, n := range s.nodes {
			if !slices.ContainsFunc(s.edges, func(e Edge) bool { return e.From == id && e.To == n.ID }) {
				continue
			}
			if in[n.ID]--; in[n.ID] == 0 {
				ready = append(ready, n.ID)
			}
		}
	}
	if len(order) == len(s.nodes) {
		return order, nil
	}
	// What the walk never reached is in a cycle, or out of the trigger's reach.
	for _, n := range s.nodes {
		if slices.Contains(order, n.ID) {
			continue
		}
		if s.reaches(trigger, n.ID) {
			return nil, fmt.Errorf("node %s is in a cycle: an automation's edges go one way", n.ID)
		}
		return nil, fmt.Errorf("node %s isn't reached from the trigger: connect it", n.ID)
	}
	panic("automation: a node is missing from the order")
}

// reaches reports whether to is reachable from from along edges.
func (s Spec) reaches(from, to string) bool {
	seen := map[string]bool{from: true}
	stack := []string{from}
	for len(stack) > 0 {
		at := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, e := range s.edges {
			if e.From == at && !seen[e.To] {
				if e.To == to {
					return true
				}
				seen[e.To] = true
				stack = append(stack, e.To)
			}
		}
	}
	return false
}

func parseNode(wn wireNode) (Node, error) {
	n := Node{ID: wn.ID, Position: wn.Position}
	switch {
	case !nodeIDPattern.MatchString(n.ID):
		return Node{}, fmt.Errorf("node id %q must be 1 to 32 lowercase letters, digits, dashes or underscores, starting with a letter or digit", n.ID)
	case math.IsNaN(n.Position.X) || math.IsNaN(n.Position.Y) || math.Abs(n.Position.X) > maxPosition || math.Abs(n.Position.Y) > maxPosition:
		return Node{}, fmt.Errorf("node %s's position is off the canvas", n.ID)
	}
	var err error
	switch wn.Kind {
	case KindTrigger:
		var t Trigger
		t, err = parseTrigger(wn.Config)
		n.Config = TriggerNode{Trigger: t}
	case KindAssistant:
		n.Config, err = parseAssistant(wn.Config)
	case KindAction:
		var a Action
		a, err = ParseAction(wn.Config)
		n.Config = ActionNode{Action: a}
	case KindCondition:
		var c Check
		c, err = parseCheck(wn.Config)
		n.Config = ConditionNode{Check: c}
	case KindNotify:
		n.Config, err = parseNotify(wn.Config)
	default:
		return Node{}, fmt.Errorf("node %s's kind is %q; it is trigger, assistant, action, condition or notify", n.ID, wn.Kind)
	}
	if err != nil {
		return Node{}, fmt.Errorf("node %s: %w", n.ID, err)
	}
	return n, nil
}

type wireAssistant struct {
	Instruction string            `json:"instruction"`
	Allow       []json.RawMessage `json:"allow,omitempty"`
}

func parseAssistant(raw json.RawMessage) (AssistantNode, error) {
	var w wireAssistant
	if err := decodeStrict(raw, &w); err != nil {
		return AssistantNode{}, fmt.Errorf("an Assistant step is {instruction, allow}: %w", err)
	}
	out := AssistantNode{Instruction: strings.TrimSpace(w.Instruction)}
	if err := checkText(out.Instruction, 1, MaxInstructionChars, "its instruction"); err != nil {
		return AssistantNode{}, err
	}
	if len(w.Allow) > MaxAllowed {
		return AssistantNode{}, fmt.Errorf("an Assistant step runs at most %d actions unasked, not %d", MaxAllowed, len(w.Allow))
	}
	for _, raw := range w.Allow {
		a, err := ParseAction(raw)
		if err != nil {
			return AssistantNode{}, fmt.Errorf("allow: %w", err)
		}
		if slices.Contains(out.Allow, a) {
			return AssistantNode{}, fmt.Errorf("allow names %s on the same target twice", a.Operation())
		}
		out.Allow = append(out.Allow, a)
	}
	return out, nil
}

type wireNotify struct {
	Message string `json:"message"`
}

func parseNotify(raw json.RawMessage) (NotifyNode, error) {
	var w wireNotify
	if err := decodeStrict(raw, &w); err != nil {
		return NotifyNode{}, fmt.Errorf("a notify step is {message}: %w", err)
	}
	out := NotifyNode{Message: strings.TrimSpace(w.Message)}
	return out, checkText(out.Message, 0, MaxMessageChars, "its message")
}

// checkText refuses text of fewer than least or more than most characters,
// or with control characters other than line breaks.
func checkText(text string, least, most int, what string) error {
	n := utf8.RuneCountInString(text)
	if !utf8.ValidString(text) || n < least || n > most ||
		strings.ContainsFunc(text, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) {
		return fmt.Errorf("%s is %d to %d characters, with no control characters", what, least, most)
	}
	return nil
}

// MarshalJSON writes the spec canonically: the same spec, the same bytes.
func (s Spec) MarshalJSON() ([]byte, error) {
	w := wireSpec{V: SpecVersion, Nodes: make([]wireNode, 0, len(s.nodes)), Edges: make([]wireEdge, 0, len(s.edges))}
	for _, n := range s.nodes {
		config, err := json.Marshal(wireConfig(n.Config))
		if err != nil {
			return nil, err
		}
		w.Nodes = append(w.Nodes, wireNode{ID: n.ID, Kind: n.Kind(), Config: config, Position: n.Position})
	}
	for _, e := range s.edges {
		w.Edges = append(w.Edges, wireEdge(e))
	}
	return json.Marshal(w)
}

// wireConfig is a node's config as the spec writes it.
func wireConfig(c Config) any {
	switch c := c.(type) {
	case TriggerNode:
		return wireTriggerOf(c.Trigger)
	case AssistantNode:
		allow := make([]json.RawMessage, 0, len(c.Allow))
		for _, a := range c.Allow {
			raw, _ := json.Marshal(wireActionOf(a)) // strings only: it can't fail
			allow = append(allow, raw)
		}
		return wireAssistant{Instruction: c.Instruction, Allow: allow}
	case ActionNode:
		return wireActionOf(c.Action)
	case ConditionNode:
		return wireCheckOf(c.Check)
	case NotifyNode:
		return wireNotify(c)
	}
	panic(fmt.Sprintf("automation: no wire form of %T", c))
}

// decodeStrict reads raw into v, refusing unknown fields and anything after
// the one value.
func decodeStrict(raw []byte, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return errors.New("it is missing")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(v); err != nil {
		return err
	}
	if decoder.More() {
		return errors.New("more than one JSON value")
	}
	return nil
}

// kindOf reads a config's discriminating field, name, before the config is
// read strictly as its kind.
func kindOf(raw json.RawMessage, name string) (string, error) {
	var head map[string]json.RawMessage
	if err := json.Unmarshal(raw, &head); err != nil || head == nil {
		return "", errors.New("its config is a JSON object")
	}
	var kind string
	if err := json.Unmarshal(head[name], &kind); err != nil {
		return "", fmt.Errorf("its config names its %s", name)
	}
	return kind, nil
}
