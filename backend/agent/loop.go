// Package agent is Founder (the Assistant in Go's names): Foundry's own
// agent, which a person talks to in the site. Each turn runs a model
// (package llm) over the thread's transcript with a closed list of Foundry's
// tools (Tools), acting as the person through a host's session
// (integration.SessionHost). It changes nothing itself: what it prepares, the
// person runs with a click (integration.ConfirmByPerson, actionSink), but for
// a new dashboard their message asked for, which runs at once (autoRun).
package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/agent/llm"
	"github.com/helixdb/helix-foundry/backend/ai"
	"github.com/helixdb/helix-foundry/backend/automation"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/integration"
	"github.com/helixdb/helix-foundry/backend/pkg/log"
	"github.com/helixdb/helix-foundry/backend/pkg/secretbox"
	"github.com/helixdb/helix-foundry/backend/ports"
	foundrystore "github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

const (
	// maxOutput is the most one call may write.
	maxOutput = llm.MaxOutput
	// toolTimeout bounds one tool call; ask waits 25 s of it.
	toolTimeout = 30 * time.Second
	// maxResultBytes is the most of one tool's result the model reads, and
	// the event shows.
	maxResultBytes = 16 << 10
	// maxTitleChars is how much of a thread's first message titles it.
	maxTitleChars = 80
	// maxCallsPerReply is how many of one reply's tool calls run; the rest
	// are answered unrun, so one reply can't queue a turn's worth of work.
	maxCallsPerReply = 8
	// maxAsksPerTurn is how many Analyst questions a turn asks: each spends
	// the workspace's questions budget.
	maxAsksPerTurn = 3
	// liveTextEvery is how often the text a model is writing is shown, as
	// TextDelta events.
	liveTextEvery = 250 * time.Millisecond
	// minContextTokens is the least of a transcript a model's window must
	// hold beside the system prompt, the tools and the answer.
	minContextTokens = 4000
)

// Store is what a turn reads and records of Foundry's store.
type Store interface {
	Reads
	ListFoundryAgentMessages(ctx context.Context, threadID string) ([]model.FoundryAgentMessage, error)
	ListFoundryAgentTurns(ctx context.Context, threadID string, limit int) ([]*model.FoundryAgentTurn, error)
	GetFoundryAgentTurn(ctx context.Context, workspaceID, id string) (*model.FoundryAgentTurn, error)
	AskFoundryAgentQuestions(ctx context.Context, turnID, workerID string, asked *model.FoundryAgentAsked, event json.RawMessage) error
	AppendFoundryAgentMessages(ctx context.Context, turnID, workerID string, messages []model.FoundryAgentMessage, keep int) error
	AppendFoundryAgentEvents(ctx context.Context, id, workerID string, events []json.RawMessage) error
	FinishFoundryAgentText(ctx context.Context, id, workerID string, step int, events []json.RawMessage) error
	RecordFoundryAgentAction(ctx context.Context, workerID string, a *model.FoundryAgentAction, maxPending int,
		card func(*model.FoundryAgentAction) (json.RawMessage, error)) (*model.FoundryAgentAction, error)
	GetFoundryAgentAction(ctx context.Context, workspaceID, id string) (*model.FoundryAgentAction, error)
	GetFoundryAgentActionByPayload(ctx context.Context, turnID, payloadHash string) (*model.FoundryAgentAction, error)
	FinishFoundryAgentAction(ctx context.Context, id string, end model.FoundryAgentActionEnd, continuation *model.FoundryAgentTurn, firstEvent json.RawMessage) error
	// MarkFoundryAutomationCause marks a sync or a load an automation's
	// Assistant step started as its run's.
	MarkFoundryAutomationCause(ctx context.Context, runID string, job model.FoundryAutomationJob) error
	GetFoundryAutomationRun(ctx context.Context, workspaceID, id string) (*model.FoundryAutomationRun, error)
	ListFoundryAutomationRunNodes(ctx context.Context, runID string) ([]*model.FoundryAutomationRunNode, error)
	GetFoundryAutomationWorkflowRun(ctx context.Context, automationRunID, ontologyID string) (*model.FoundryWorkflowRun, error)
	StartFoundryAutomationWorkflowRun(ctx context.Context, start model.FoundryAutomationWorkflowStart) (*model.FoundryWorkflowRun, error)
}

// Resolver is where a workspace's model calls go; *ai.Resolver resolves it.
type Resolver interface {
	Resolve(ctx context.Context, workspaceID string) (ai.Resolved, error)
}

// Models binds the model a turn calls (the workspace's ask model), its price
// and its window; llm.New, or a test's.
type Models func(r ai.Resolved, catalog *ai.Catalog) (llm.Binding, error)

// Runner runs the attempts of turns.
type Runner struct {
	DB       Store
	Sessions integration.SessionHost
	AI       Resolver
	Models   Models
	Catalog  *ai.Catalog
	// Keys seals the confirmations of the actions turns prepare.
	Keys *secretbox.Keyring
	// Databases says whose a database is, for a create_ontology on one;
	// nil refuses those.
	Databases ports.Databases
	Cfg       config.AssistantConfig
	WorkerID  string
	Now       func() time.Time
}

// Failure is why an attempt didn't end its turn, worded for the person:
// Permanent fails the turn, anything else is retried.
type Failure struct {
	Message   string
	Permanent bool
	Err       error
}

func (f *Failure) Error() string {
	if f.Err != nil {
		return f.Message + ": " + f.Err.Error()
	}
	return f.Message
}

func (f *Failure) Unwrap() error { return f.Err }

// ErrLost is an attempt that lost its turn's lease, to a cancel, another
// worker or the thread's deletion: it records nothing more.
var ErrLost = errors.New("agent: the turn's lease was lost")

// Attempt is what an attempt spent, and how it ended the turn when it did.
type Attempt struct {
	Spend model.FoundryGraphJobSpend
	Steps int
	// Stop is why the turn ended; empty for an attempt that failed.
	Stop StopReason
}

// turnRun is one attempt's state.
type turnRun struct {
	r    *Runner
	turn *model.FoundryAgentTurn
	// automation is an automation's turn's step, and uses its run's uses of
	// ontologies; both nil for a person's turn.
	automation *automationStep
	uses       *AutomationUses
	model      llm.Model
	pricer     llm.Pricer
	client     *mcp.ClientSession
	offered    map[string]*mcp.Tool
	tools      []llm.Tool
	// fixedTokens is about what every call sends besides the transcript: the
	// system prompt and the tools' definitions.
	fixedTokens int
	// budget is about how much of the transcript a call sends: the
	// Assistant's cap, within the model's window where it has one.
	budget  int
	all     []entry
	started time.Time
	out     *Attempt
	// asked is whether the turn asked its person (toolAskPerson), in this
	// attempt or one before: it ends once that call's result is committed.
	asked bool
}

// Run runs an attempt of turn:
//  1. resolve the workspace's provider and its ask model, which the turn runs
//     on, unless the setting's tool check found the Assistant can't;
//  2. open the person's session, confirming as the person, and serve its tools
//     to the model through an in-memory MCP client, Tools alone;
//  3. read the thread's transcript, adding the turn's own message once;
//  4. call the model and run the tools it calls, committing each message and
//     its events before the next call, until it stops, or the turn reaches
//     its steps, wall time or budget.
//
// An attempt resumes what its turn's transcript committed: calls a stopped
// attempt left unanswered run again (an ask with the same client token, a
// prepare that finds its action). Its error is a *Failure, ErrLost, or
// another error to retry on.
func (r *Runner) Run(ctx context.Context, turn *model.FoundryAgentTurn) (*Attempt, error) {
	out := &Attempt{}
	resolved, err := r.AI.Resolve(ctx, turn.WorkspaceID)
	if err != nil {
		c := ai.ResolveFailure(err)
		return out, &Failure{Message: c.Message, Permanent: c.Permanent, Err: err}
	}
	ask := resolved.Settings.Models.Ask
	if check := resolved.Report.Assistant; !check.Runs() {
		return out, &Failure{Message: checkFailure(check, resolved.Label(), ask.Name), Permanent: true, Err: fmt.Errorf("the setting's tool check: %s", check)}
	}
	bound, err := r.Models(resolved, r.Catalog)
	if err != nil {
		return out, &Failure{Message: "Founder can't call the workspace's model; an owner can check Settings › AI models.", Permanent: true, Err: err}
	}
	sink := &actionSink{db: r.DB, keys: r.Keys, turn: turn, workerID: r.WorkerID, maxPending: r.Cfg.MaxPendingActions, sessions: r.Sessions}
	var step *automationStep
	if turn.Kind == model.FoundryAgentTurnAutomation {
		if step, err = r.automationStep(ctx, turn); err != nil {
			return out, err
		}
		sink.approved = step.allow
	}
	session, err := r.Sessions.Session(ctx, ports.Actor{Source: ports.SourceUser, UserID: turn.UserID, WorkspaceID: turn.WorkspaceID},
		integration.ConfirmByPerson{Sink: sink})
	switch status.Code(err) {
	case codes.OK:
	case codes.PermissionDenied, codes.NotFound, codes.Unauthenticated:
		return out, &Failure{Message: "You no longer have access to this workspace.", Permanent: true, Err: err}
	default:
		return out, &Failure{Message: "Your access to this workspace couldn't be read; Helix will retry", Err: err}
	}
	// A card is drawn as its person reads the workspace.
	sink.cards = SessionCards(r.DB, r.Databases, session, turn.WorkspaceID)
	t := &turnRun{r: r, turn: turn, automation: step, model: bound.Model, pricer: bound.Pricer, started: r.Now(), out: out, asked: turn.Asked != nil}
	if step != nil {
		t.uses = NewAutomationUses(r.DB, step.run, step.name, step.spec)
	}
	client, offered, err := serve(session, func(server *mcp.Server) {
		t.reportTool(server)
		asker{db: r.DB, session: session, turn: turn, workerID: r.WorkerID}.register(server)
	})
	if err != nil {
		return out, &Failure{Message: "Founder's tools couldn't start; Helix will retry", Err: err}
	}
	defer func() { _ = client.Close() }()
	t.client, t.offered = client, offered
	stored, err := r.DB.ListFoundryAgentMessages(ctx, turn.ThreadID)
	if err != nil {
		return out, err
	}
	all, err := entries(stored)
	if err != nil {
		return out, &Failure{Message: "The thread's transcript can't be read", Permanent: true, Err: err}
	}
	t.all = all
	fixed := len(t.system())
	for _, tool := range offered {
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			return out, err
		}
		schema = withoutWorkflowRun(schema)
		t.tools = append(t.tools, llm.Tool{Name: tool.Name, Description: tool.Description, InputSchema: schema})
		fixed += len(tool.Name) + len(tool.Description) + len(schema)
	}
	t.fixedTokens = fixed / 4
	t.budget = r.Cfg.ContextTokens
	if bound.Window > 0 {
		// Estimates count four bytes a token, which JSON beats: three
		// quarters of the window's room holds a transcript of three. A
		// server that loads the window cuts what passes it without a word.
		room := (bound.Window-maxOutput)*3/4 - t.fixedTokens
		if room < minContextTokens {
			return out, &Failure{Message: fmt.Sprintf("The model's context window, %d tokens, is too small for Founder. An owner can choose a larger one in Settings › AI models.", bound.Window),
				Permanent: true, Err: fmt.Errorf("window %d leaves %d tokens", bound.Window, room)}
		}
		t.budget = min(t.budget, room)
	}
	// One order every call, so the cached prefix holds.
	slices.SortFunc(t.tools, func(a, b llm.Tool) int { return strings.Compare(a.Name, b.Name) })
	if err := t.event(ctx, Started{Model: ask.Name, Attempt: turn.Attempts}); err != nil {
		return out, err
	}
	if !slices.ContainsFunc(all, func(e entry) bool { return e.turnID == turn.ID }) {
		if err := t.open(ctx); err != nil {
			return out, err
		}
	}
	return out, t.loop(ctx)
}

// serve puts the session's tools, and own's, on a server of their own,
// Tools alone (toolServer), and connects a client to it in memory. offered
// is what the client lists.
func serve(session integration.Session, own func(*mcp.Server)) (*mcp.ClientSession, map[string]*mcp.Tool, error) {
	server, offered, err := toolServer(session, own)
	if err != nil {
		return nil, nil, err
	}
	serverSide, clientSide := mcp.NewInMemoryTransports()
	if _, err := server.Connect(session.Ctx, serverSide, nil); err != nil {
		return nil, nil, err
	}
	client, err := mcp.NewClient(&mcp.Implementation{Name: "helix-foundry-assistant", Version: PromptVersion}, nil).Connect(session.Ctx, clientSide, nil)
	if err != nil {
		return nil, nil, err
	}
	return client, offered, nil
}

// toolServer is a server of the session's tools, and own's (nil for none),
// Tools alone, and those tools by name: every other tool the session
// registers is removed before anyone lists them.
func toolServer(session integration.Session, own func(*mcp.Server)) (*mcp.Server, map[string]*mcp.Tool, error) {
	server := mcp.NewServer(&mcp.Implementation{Name: "helix-foundry-assistant", Version: PromptVersion}, nil)
	session.Service.AddTools(server, session.Identity)
	if session.HostTools != nil {
		session.HostTools(server)
	}
	if own != nil {
		own(server)
	}
	// The SDK lists a server's tools only to a client: list them to one of
	// its own, in memory.
	serverSide, clientSide := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(session.Ctx, serverSide, nil)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = serverSession.Close() }()
	client, err := mcp.NewClient(&mcp.Implementation{Name: "helix-foundry-assistant", Version: PromptVersion}, nil).Connect(session.Ctx, clientSide, nil)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = client.Close() }()
	listed, err := client.ListTools(session.Ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	var outside []string
	offered := map[string]*mcp.Tool{}
	for _, tool := range listed.Tools {
		if Tools[tool.Name] {
			offered[tool.Name] = tool
		} else {
			outside = append(outside, tool.Name)
		}
	}
	server.RemoveTools(outside...)
	return server, offered, nil
}

// open adds the turn's own message to the transcript: the person's text with
// the page they were on, checked against the workspace, their answers, or
// how their click on an action ended.
func (t *turnRun) open(ctx context.Context) error {
	text, err := openingText(ctx, t.r.DB, t.turn)
	if err != nil {
		return err
	}
	return t.commit(ctx, llm.Message{Role: llm.User, Content: []llm.Block{llm.Text{Text: text}}})
}

// openingText is what a turn opens its model's part of the transcript with:
// the person's text with the page they were on, checked against the
// workspace, their answers to the questions the turn before asked, or how
// their click on an action ended.
func openingText(ctx context.Context, db Store, turn *model.FoundryAgentTurn) (string, error) {
	switch turn.Kind {
	case model.FoundryAgentTurnMessage:
		return messageText(turn.Input, page(ctx, db, turn)), nil
	case model.FoundryAgentTurnAutomation:
		// The step's instruction, with what fired the run, as the
		// foundry-automation-worker wrote it.
		return turn.Input, nil
	case model.FoundryAgentTurnAnswers:
		asking, err := db.GetFoundryAgentTurn(ctx, turn.WorkspaceID, turn.Input)
		if err == nil && asking.Asked == nil {
			err = errors.New("the turn answered asked nothing")
		}
		if err != nil {
			return "", &Failure{Message: "The questions these answers answer can't be read", Permanent: true, Err: err}
		}
		return answersText(asking.Asked, turn.Answers), nil
	case model.FoundryAgentTurnActionResult:
		a, err := db.GetFoundryAgentAction(ctx, turn.WorkspaceID, turn.Input)
		if err != nil {
			return "", &Failure{Message: "The action this turn goes on from can't be read", Permanent: true, Err: err}
		}
		return actionResultText(ActionResult{ActionID: a.ID, Operation: a.Operation, Status: string(a.Status), Result: a.Result, Error: deref(a.Error)}), nil
	}
	return "", &Failure{Message: "The turn is of no kind Founder runs", Permanent: true, Err: fmt.Errorf("kind %q", turn.Kind)}
}

// page is the turn's page context as the model is told it: each id the
// workspace's, with the ontology's and the dashboard's names, and the
// person's time zone when it is one; nil when it names nothing that is.
func page(ctx context.Context, db Reads, turn *model.FoundryAgentTurn) *shownPage {
	var p PageContext
	if len(turn.PageContext) == 0 || json.Unmarshal(turn.PageContext, &p) != nil {
		return nil
	}
	var out shownPage
	if p.OntologyID != "" {
		if o, err := db.GetFoundryOntology(ctx, turn.WorkspaceID, p.OntologyID); err == nil && o.DeletedAt == nil {
			out.OntologyID, out.OntologyName = o.ID, o.Name
			if p.LoadID != "" {
				if l, err := db.GetFoundryLoad(ctx, o.ID, p.LoadID); err == nil && l.Status != model.FoundryLoadDeleted {
					out.LoadID = l.ID
				}
			}
		}
	}
	if p.DashboardID != "" {
		if d, err := db.GetFoundryDashboard(ctx, turn.WorkspaceID, p.DashboardID); err == nil {
			out.DashboardID, out.DashboardName = d.ID, d.Name
		}
	}
	out.CreatingAutomation = p.NewAutomation
	if _, err := automation.Zone(p.TimeZone); err == nil {
		out.TimeZone = p.TimeZone
	}
	if out == (shownPage{}) {
		return nil
	}
	return &out
}

// loop calls the model and runs its calls until the turn ends.
func (t *turnRun) loop(ctx context.Context) error {
	for {
		if last := t.all[len(t.all)-1]; last.turnID == t.turn.ID && last.msg.Role == llm.Assistant {
			uses := toolUses(last.msg)
			if len(uses) == 0 {
				// A stopped attempt committed the model's last word: the turn is done.
				return t.finish(ctx, StopAnswered)
			}
			if err := t.runTools(ctx, uses); err != nil {
				return err
			}
			continue
		}
		if t.asked {
			// The person answers next: no call until then.
			return t.finish(ctx, StopAsked)
		}
		messages := prompt(t.all, t.turn.ID, t.budget)
		in := t.fixedTokens
		for _, m := range messages {
			in += estTokens(m)
		}
		// The turn's own spend: a dead attempt's reservation, which the month
		// paid, isn't some of it.
		spent := t.turn.Spend.USDMicros - t.turn.LostMicros + t.out.Spend.USDMicros
		switch {
		case in-t.fixedTokens > t.budget:
			// The turn running is never left out, so it alone may pass the
			// budget: a call now would be cut, or refused, by the model's window.
			return t.finish(ctx, StopContext)
		case t.turn.Steps+t.out.Steps >= t.r.Cfg.MaxSteps:
			return t.finish(ctx, StopSteps)
		case t.outOfTime():
			return t.finish(ctx, StopWallTime)
		case spent+t.pricer.Micros(llm.Usage{In: int64(in), Out: maxOutput}) > t.turn.MaxUSDMicros:
			return t.finish(ctx, StopBudget)
		}
		answer, stop, dropped, streamed, err := t.call(ctx, messages)
		if err != nil {
			return err
		}
		if len(answer.Content) > 0 {
			if err := t.commit(ctx, answer); err != nil {
				return err
			}
		}
		if err := t.said(ctx, answer, streamed); err != nil {
			return err
		}
		for _, use := range dropped {
			why := NotRunInput
			if stop == StopMaxTokens {
				why = NotRunLength
			}
			if err := t.notRun(ctx, use, why); err != nil {
				return err
			}
		}
		if len(answer.Content) == 0 {
			// Nothing to keep: a refusal before any word, say.
			return t.finish(ctx, stop)
		}
		if stop != StopAnswered || len(toolUses(answer)) == 0 {
			return t.finish(ctx, stop)
		}
	}
}

// outOfTime reports an attempt past the turn's wall time.
func (t *turnRun) outOfTime() bool {
	return t.r.Now().Sub(t.started) >= time.Duration(t.r.Cfg.MaxWallSecs)*time.Second
}

// step is the number of the turn's model call running, or last run.
func (t *turnRun) step() int { return t.turn.Steps + t.out.Steps }

// call makes one model call, adding what it used to the attempt. A tool call
// that isn't JSON, or that a reply cut off at its output's limit was writing,
// is left out of the answer, and returned as dropped: it never runs. The text
// the model writes is shown as it comes, every liveTextEvery, as TextDelta
// events of the call's step; streamed reports whether any were.
func (t *turnRun) call(ctx context.Context, messages []llm.Message) (llm.Message, StopReason, []llm.ToolUse, bool, error) {
	answer := llm.Message{Role: llm.Assistant}
	stop := StopAnswered
	var dropped []llm.ToolUse
	streamed := false
	t.out.Steps++
	var live strings.Builder
	shown := t.r.Now()
	for chunk, err := range t.model.Stream(ctx, llm.Request{System: t.system(), Messages: messages, Tools: t.tools, MaxOutput: maxOutput}) {
		if err != nil {
			var refused *llm.Error
			if errors.As(err, &refused) && !refused.Retryable() {
				message := "The model provider refused Founder's call"
				if refused.Kind == llm.ErrAuth {
					message = "The model provider refused the workspace's key. An owner can check it in Settings › AI models."
				}
				return answer, "", nil, streamed, &Failure{Message: message, Permanent: true, Err: err}
			}
			return answer, "", nil, streamed, &Failure{Message: "The model provider is unavailable; Helix will retry", Err: err}
		}
		switch c := chunk.(type) {
		case llm.TextDelta:
			live.WriteString(c.Text)
			if now := t.r.Now(); now.Sub(shown) >= liveTextEvery {
				// Live text is for show: a write that failed keeps its text
				// for the next, and the call streams on, since the step's text
				// event replaces it. Only a lost lease ends the call.
				shown, streamed = now, true
				switch err := t.event(ctx, TextDelta{Step: t.step(), Text: live.String()}); {
				case errors.Is(err, ErrLost):
					return answer, "", nil, streamed, err
				case err != nil:
					log.From(ctx).Warn("the Assistant's live text couldn't be written; it streams on", "turn", t.turn.ID, "err", err)
				default:
					live.Reset()
				}
			}
		case llm.Text:
			answer.Content = append(answer.Content, c)
		case llm.ToolUse:
			if json.Valid(c.Input) {
				answer.Content = append(answer.Content, c)
			} else {
				dropped = append(dropped, c)
			}
		case llm.Reasoning:
			answer.Content = append(answer.Content, c)
		case llm.Usage:
			t.out.Spend.USDMicros += t.pricer.Micros(c)
			t.out.Spend.InputTokens += c.In + c.CacheRead + c.CacheWrite
			t.out.Spend.OutputTokens += c.Out
		case llm.Stop:
			switch c.Reason {
			case llm.StopMaxTokens:
				stop = StopMaxTokens
			case llm.StopRefusal:
				stop = StopRefused
			case llm.StopContext:
				stop = StopContext
			}
		}
	}
	if stop == StopMaxTokens {
		// The turn ends here, so no call of this reply would run.
		answer.Content = slices.DeleteFunc(answer.Content, func(b llm.Block) bool {
			use, ok := b.(llm.ToolUse)
			if ok {
				dropped = append(dropped, use)
			}
			return ok
		})
	}
	return answer, stop, dropped, streamed, nil
}

// said shows the person the text of the step's answer, as it is final, and
// drops the TextDelta events of the step it supersedes; the transcript holds
// the answer either way.
func (t *turnRun) said(ctx context.Context, answer llm.Message, streamed bool) error {
	var events []json.RawMessage
	for _, b := range answer.Content {
		if said, ok := b.(llm.Text); ok && strings.TrimSpace(said.Text) != "" {
			raw, err := MarshalEvent(Said{Step: t.step(), Text: said.Text})
			if err != nil {
				return err
			}
			events = append(events, raw)
		}
	}
	if !streamed && len(events) == 0 {
		return nil
	}
	err := t.r.DB.FinishFoundryAgentText(ctx, t.turn.ID, t.r.WorkerID, t.step(), events)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return ErrLost
	}
	return err
}

// checkFailure words a setting's failed tool check for the person: the
// model, and what an owner can do.
func checkFailure(check ai.AssistantCheck, provider, model string) string {
	switch check {
	case ai.AssistantAuth:
		return provider + " refused the workspace's key for Founder. An owner can check it in Settings › AI models."
	case ai.AssistantNoToolCall:
		return "The workspace's model, " + model + ", doesn't call tools, which Founder needs. An owner can choose another in Settings › AI models."
	default:
		return provider + " refused Founder's calls to " + model + ". An owner can choose another model in Settings › AI models."
	}
}

// runTools runs the model's calls, in order, and commits their results as one
// message. A tool the model wasn't offered isn't called, nor are the calls
// past maxCallsPerReply, an ask past maxAsksPerTurn, anything once the
// turn's wall time is up, or anything but toolAskPerson once the turn asked
// its person (a second ask is the tool's to refuse): each is answered with
// why.
func (t *turnRun) runTools(ctx context.Context, uses []llm.ToolUse) error {
	asked := 0
	for _, e := range t.all[:len(t.all)-1] {
		if e.turnID == t.turn.ID {
			asked += len(slices.DeleteFunc(toolUses(e.msg), func(u llm.ToolUse) bool { return u.Name != toolAsk }))
		}
	}
	results := make([]llm.Block, 0, len(uses))
	for i, use := range uses {
		// Why it isn't run, for the person, and for the model.
		var why NotRunReason
		var told string
		switch {
		case i >= maxCallsPerReply:
			why, told = NotRunCalls, fmt.Sprintf("Not run: at most %d tool calls of one reply run. Call it again if you still need it.", maxCallsPerReply)
		case t.outOfTime():
			why, told = NotRunTime, "Not run: the turn ran out of time. Tell the person what you found so far."
		case use.Name == toolAsk && asked >= maxAsksPerTurn:
			why, told = NotRunAsks, fmt.Sprintf("Not run: a turn asks at most %d questions. Answer with what you have, or let the person ask again.", maxAsksPerTurn)
		case t.asked && use.Name != toolAskPerson:
			why, told = NotRunAsked, "Not run: you asked the person, which ends your turn. Their answers come as the next message."
		}
		if use.Name == toolAsk {
			asked++
		}
		if why != "" {
			if err := t.notRun(ctx, use, why); err != nil {
				return err
			}
			results = append(results, llm.ToolResult{ToolUseID: use.ID, Content: told, IsError: true})
			continue
		}
		tool := t.offered[use.Name]
		if err := t.event(ctx, ToolCalled{ToolUseID: use.ID, Name: use.Name, Title: t.title(use.Name), Input: use.Input}); err != nil {
			return err
		}
		content, output, failed := t.callTool(ctx, use, tool != nil)
		shown := ToolReturned{ToolUseID: use.ID, Name: use.Name, IsError: failed}
		if failed {
			shown.Error = content
		} else {
			if raw, err := json.Marshal(output); err == nil && len(raw) <= maxResultBytes {
				shown.Output = json.RawMessage(raw)
			} else {
				shown.Truncated = true
			}
			shown.Question = questionOf(use.Name, use.Input, output)
		}
		if err := t.event(ctx, shown); err != nil {
			return err
		}
		t.asked = t.asked || use.Name == toolAskPerson && !failed
		results = append(results, llm.ToolResult{ToolUseID: use.ID, Content: content, IsError: failed})
	}
	return t.commit(ctx, llm.Message{Role: llm.User, Content: results})
}

// title is a tool's name for people: its title, else its name.
func (t *turnRun) title(name string) string {
	if tool := t.offered[name]; tool != nil {
		return cmp.Or(tool.Title, name)
	}
	return name
}

// notRun shows the person a call the model wrote that a limit kept from
// running, and why: the model is told the same.
func (t *turnRun) notRun(ctx context.Context, use llm.ToolUse, why NotRunReason) error {
	input := use.Input
	if !json.Valid(input) {
		// Shown as text, so the event stays JSON.
		input, _ = json.Marshal(string(input))
	}
	if err := t.event(ctx, ToolCalled{ToolUseID: use.ID, Name: use.Name, Title: t.title(use.Name), Input: input}); err != nil {
		return err
	}
	return t.event(ctx, ToolReturned{ToolUseID: use.ID, Name: use.Name, IsError: true, NotRun: why})
}

// callTool runs one call within toolTimeout: what the model reads of its
// result (cut at maxResultBytes), its structured output, and whether it failed.
func (t *turnRun) callTool(ctx context.Context, use llm.ToolUse, offered bool) (string, any, bool) {
	if !offered {
		return "No tool named " + use.Name + " is available to you.", nil, true
	}
	input := use.Input
	if use.Name == toolAsk {
		forced, err := withAskClientToken(input, t.turn.ID, use.ID)
		if err != nil {
			return "The input isn't a JSON object.", nil, true
		}
		input = forced
	}
	var args map[string]any
	if err := json.Unmarshal(input, &args); err != nil || args == nil {
		return "The input isn't a JSON object.", nil, true
	}
	// Only Foundry names the workflow run a call is recorded under: an
	// automation's for its turns, none for a person's.
	if t.uses == nil {
		delete(args, workflowRunField)
	} else if err := t.uses.Called(ctx, use.Name, args); err != nil {
		log.From(ctx).Warn("automation workflows: a use of an ontology went unrecorded", "turn", t.turn.ID, "tool", use.Name, "err", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()
	result, err := t.client.CallTool(callCtx, &mcp.CallToolParams{Name: use.Name, Arguments: args})
	if err != nil {
		return "The tool call failed: " + err.Error(), nil, true
	}
	var texts []string
	for _, c := range result.Content {
		if text, ok := c.(*mcp.TextContent); ok {
			texts = append(texts, text.Text)
		}
	}
	return cut(strings.Join(texts, "\n")), result.StructuredContent, result.IsError
}

// cut is text at most maxResultBytes long, on a rune boundary, saying so when cut.
func cut(text string) string {
	if len(text) <= maxResultBytes {
		return text
	}
	end := maxResultBytes
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + "\n[Cut at 16 KiB: ask for less, or narrower.]"
}

// commit adds a message to the transcript, under the turn's lease.
func (t *turnRun) commit(ctx context.Context, m llm.Message) error {
	content, err := llm.MarshalBlocks(m.Content)
	if err != nil {
		return err
	}
	err = t.r.DB.AppendFoundryAgentMessages(ctx, t.turn.ID, t.r.WorkerID, []model.FoundryAgentMessage{
		{TurnID: t.turn.ID, Role: model.FoundryAgentRole(m.Role), Content: content, EstTokens: estTokens(m)},
	}, keepMessages)
	if errors.Is(err, foundrystore.ErrNotFound) {
		return ErrLost
	}
	if err != nil {
		return err
	}
	t.all = append(t.all, entry{turnID: t.turn.ID, msg: m})
	return nil
}

// event shows the person e, under the turn's lease.
func (t *turnRun) event(ctx context.Context, e Event) error {
	raw, err := MarshalEvent(e)
	if err != nil {
		return err
	}
	err = t.r.DB.AppendFoundryAgentEvents(ctx, t.turn.ID, t.r.WorkerID, []json.RawMessage{raw})
	if errors.Is(err, foundrystore.ErrNotFound) {
		return ErrLost
	}
	return err
}

// finish ends the turn with why, for the person.
func (t *turnRun) finish(ctx context.Context, why StopReason) error {
	t.out.Stop = why
	return t.event(ctx, Finished{Status: string(model.FoundryAgentTurnDone), Reason: why})
}

// Title is a thread's title from its first message: its first line, cut.
func Title(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	line = strings.TrimSpace(line)
	if utf8.RuneCountInString(line) <= maxTitleChars {
		return line
	}
	runes := []rune(line)
	return strings.TrimSpace(string(runes[:maxTitleChars-1])) + "…"
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
