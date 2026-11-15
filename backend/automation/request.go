package automation

import (
	"fmt"
	"strings"
)

// Request is a run of the automation named name, in words, as the workflow
// runs that record its uses of ontologies take it for their request: what
// fired it (fired, its trigger's output) and each of its steps, in order.
// names names the connectors, ontologies, document sets and dashboards its
// steps act on, by id; an id it lacks is written as it is.
//
//	automation.Request("Weekly digest", "Its schedule fired.", spec, map[string]string{"01ONT": "Sales"})
//	// The automation "Weekly digest" ran. Its schedule fired.
//	// Its steps, in order:
//	// 1. Assistant step "ask": Summarise the week's deals.
//	// 2. Action step "load": load the current version of the ontology "Sales".
func Request(name, fired string, s Spec, names map[string]string) string {
	named := func(what, id string) string {
		if n, ok := names[id]; ok && n != "" {
			return fmt.Sprintf("the %s %q", what, n)
		}
		return fmt.Sprintf("the %s %s", what, id)
	}
	action := func(a Action) string {
		switch a := a.(type) {
		case SyncConnector:
			return "sync " + named("connector", a.ConnectorID)
		case CreateLoad:
			return "load the current version of " + named("ontology", a.OntologyID)
		case PublishDashboard:
			return "publish " + named("dashboard", a.DashboardID) + " again at its link"
		}
		panic(fmt.Sprintf("automation: no words for %T", a))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "The automation %q ran.", name)
	if fired = strings.TrimSpace(fired); fired != "" {
		b.WriteString(" " + fired)
	}
	b.WriteString("\nIts steps, in order:")
	step := 0
	for _, id := range s.order {
		n, _ := s.Node(id)
		var text string
		switch c := n.Config.(type) {
		case TriggerNode:
			continue
		case AssistantNode:
			text = fmt.Sprintf("Assistant step %q: %s", id, c.Instruction)
			if len(c.Allow) > 0 {
				allowed := make([]string, len(c.Allow))
				for i, a := range c.Allow {
					allowed[i] = action(a)
				}
				text += " (It may " + strings.Join(allowed, "; ") + " unasked.)"
			}
		case ActionNode:
			text = fmt.Sprintf("Action step %q: %s.", id, action(c.Action))
		case ConditionNode:
			text = fmt.Sprintf("Condition %q: go on one way if %s, the other way if not.", id, checkWords(c.Check))
		case NotifyNode:
			text = fmt.Sprintf("Notify step %q: email its owner", id)
			if c.Message != "" {
				text += fmt.Sprintf(" %q", c.Message)
			}
			text += ", with what the Assistant steps before it said."
		}
		step++
		fmt.Fprintf(&b, "\n%d. %s", step, text)
	}
	return b.String()
}

// checkWords is what a condition checks, in words.
func checkWords(c Check) string {
	switch c := c.(type) {
	case AnswerIs:
		if c.Yes {
			return "the step before answered yes"
		}
		return "the step before answered no"
	case CountIs:
		op := map[CountOp]string{AtLeast: "at least", AtMost: "at most", Equals: "exactly"}[c.Op]
		return fmt.Sprintf("the step before counted %s %d", op, c.Value)
	}
	panic(fmt.Sprintf("automation: no words for %T", c))
}
