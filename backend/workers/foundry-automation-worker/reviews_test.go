package foundryautomationworker

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/agent"
	"github.com/helixdb/helix-foundry/backend/automation"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// tellsOnReviews emails its owner once at least 50 cards wait in the Sales
// graph's current load.
var tellsOnReviews = spec(`[{"from":"start","to":"mail"}]`,
	node("start", "trigger", `{"kind":"reviews_waiting","ontology_id":"ont1","at_least":50}`), node("mail", "notify", `{"message":""}`))

// reviews is a watched reviews_waiting automation and the state its store
// reads: the ontology's current load and its open cards, counted at dirty.
type reviews struct {
	f     fixture
	a     *model.FoundryAutomation
	load  string
	open  int64
	dirty time.Time
}

func newReviews(t *testing.T) *reviews {
	t.Helper()
	f := newFixture(t)
	cursor := model.FoundryAutomationCursor{At: f.task.now().Add(-time.Hour)}
	a := f.watching("auto1", tellsOnReviews, cursor)
	f.db.watched = []*model.FoundryAutomation{a}
	return &reviews{f: f, a: a, load: "load1", dirty: cursor.At.Add(-time.Minute)}
}

// count says the load's index counted open cards, later than any count before.
func (r *reviews) count(load string, open int64) {
	r.load, r.open, r.dirty = load, open, r.dirty.Add(time.Minute)
}

// poll watches once, the store finding the load as FindFoundryAutomationEnded
// does (at the later of its dirty_at and the cursor), and moves the cursor as
// FireFoundryAutomation does, unless the fire waited for a live run. It is
// the fire it made, or nil when it only looked.
func (r *reviews) poll(t *testing.T) *model.FoundryAutomationFire {
	t.Helper()
	at := r.dirty
	if r.a.Cursor.At.After(at) {
		at = r.a.Cursor.At
	}
	open := r.open
	r.f.db.ended = []model.FoundryAutomationEnded{{Cursor: model.FoundryAutomationCursor{At: at, ID: r.load}, Rows: &open, Cause: r.f.db.causes[r.load]}}
	fires, touched := len(r.f.db.fires), len(r.f.db.touched)
	r.f.task.watch(context.Background())
	if len(r.f.db.fires) == fires {
		require.Len(t, r.f.db.touched, touched+1, "it looked")
		return nil
	}
	fire := r.f.lastFire(t)
	if r.f.db.live && fire.Run != nil && fire.Skip == "" {
		return &fire
	}
	advance := fire.Advance.(model.FoundryCursorAdvance)
	require.Equal(t, *r.a.Cursor, advance.From, "the fire moves the cursor it read")
	to := advance.To
	r.a.Cursor = &to
	return &fire
}

// key is the run key a fire on the current load makes now.
func (r *reviews) key() string {
	return "reviews_waiting:" + r.load + ":" + strconv.FormatInt(r.a.Cursor.At.UnixMicro(), 10)
}

// It fires once the current load has enough cards, once: a second poll on
// the same load runs nothing, however many more wait.
func TestReviewsWaitingFiresOnceAtTheThreshold(t *testing.T) {
	r := newReviews(t)
	r.count("load1", 50)
	key := r.key()
	fire := r.poll(t)
	require.NotNil(t, fire)
	require.NotNil(t, fire.Run)
	assert.Empty(t, fire.Skip)
	assert.Equal(t, key, fire.Run.TriggerKey)
	assert.Equal(t, model.FoundryAutomationCursor{At: r.dirty, ID: "load1"}, *r.a.Cursor, "the cursor names the load it fired on")

	r.count("load1", 80)
	assert.Nil(t, r.poll(t), "it fired on this load")
	assert.Len(t, r.f.db.fires, 1)
}

// Below the threshold it only looks, its cursor where it was; once the count
// rises past it, it fires.
func TestReviewsWaitingWaitsForTheThreshold(t *testing.T) {
	r := newReviews(t)
	before := *r.a.Cursor
	r.count("load1", 49)
	assert.Nil(t, r.poll(t))
	assert.Equal(t, before, *r.a.Cursor, "nothing moved")

	r.count("load1", 51)
	fire := r.poll(t)
	require.NotNil(t, fire)
	require.NotNil(t, fire.Run)
	assert.Equal(t, "reviews_waiting:load1:"+strconv.FormatInt(before.At.UnixMicro(), 10), fire.Run.TriggerKey)
}

// After it fired, a count that falls below the threshold arms it again with
// no run, and a count that rises past it again fires again, under a new key.
func TestReviewsWaitingFiresAgainAfterTheCountFellAndRose(t *testing.T) {
	r := newReviews(t)
	r.count("load1", 60)
	first := r.poll(t)
	require.NotNil(t, first.Run)

	r.count("load1", 3)
	rearm := r.poll(t)
	require.NotNil(t, rearm)
	assert.Nil(t, rearm.Run, "armed again, with no run")
	assert.Equal(t, model.FoundryAutomationCursor{At: r.dirty}, *r.a.Cursor, "armed: the cursor names no load")
	assert.Nil(t, r.poll(t), "armed, with too few: it only looks")

	r.count("load1", 75)
	second := r.poll(t)
	require.NotNil(t, second.Run)
	assert.NotEqual(t, first.Run.TriggerKey, second.Run.TriggerKey, "each arming has its own key")
	assert.Equal(t, "load1", r.a.Cursor.ID)
}

// A new current load fires on its own, even while the load before it fired.
func TestReviewsWaitingFiresForANewLoad(t *testing.T) {
	r := newReviews(t)
	r.count("load1", 60)
	require.NotNil(t, r.poll(t).Run)

	r.count("load2", 60)
	fire := r.poll(t)
	require.NotNil(t, fire.Run)
	assert.Contains(t, fire.Run.TriggerKey, "reviews_waiting:load2:")
	assert.Equal(t, "load2", r.a.Cursor.ID)
}

// A pause and a resume reset the cursor (the store's resume), so a load that
// still has enough cards fires once more.
func TestReviewsWaitingFiresOnceAfterAResume(t *testing.T) {
	r := newReviews(t)
	r.count("load1", 60)
	require.NotNil(t, r.poll(t).Run)
	assert.Nil(t, r.poll(t))

	r.a.Cursor = &model.FoundryAutomationCursor{At: r.f.task.now()}
	fire := r.poll(t)
	require.NotNil(t, fire.Run)
	assert.Equal(t, "reviews_waiting:load1:"+strconv.FormatInt(r.f.task.now().UnixMicro(), 10), fire.Run.TriggerKey)
	assert.Nil(t, r.poll(t), "once")
}

// While its run before is still going, the fire waits, cursor unmoved, and
// fires under the same key once that run ended.
func TestReviewsWaitingWaitsForALiveRun(t *testing.T) {
	r := newReviews(t)
	r.count("load1", 60)
	r.f.db.live = true
	waiting := r.poll(t)
	require.NotNil(t, waiting.Run)
	assert.Empty(t, r.a.Cursor.ID, "it waits, unmoved")

	r.f.db.live = false
	fire := r.poll(t)
	require.NotNil(t, fire.Run)
	assert.Equal(t, waiting.Run.TriggerKey, fire.Run.TriggerKey)
	assert.Equal(t, "load1", r.a.Cursor.ID)
}

// A load its own run started is recorded skipped, saying the load, once: the
// cursor names it as a fire's would.
func TestReviewsWaitingSkipsALoadItsOwnRunStarted(t *testing.T) {
	r := newReviews(t)
	r.f.db.causes["own-load"] = &model.FoundryAutomationCause{RunID: "run0", AutomationID: "auto1", Depth: 1}
	r.count("own-load", 60)
	fire := r.poll(t)
	require.NotNil(t, fire.Run, "recorded, so its owner sees why it didn't run")
	assert.Equal(t, "Skipped: the load was started by this workflow itself; running on it would loop.", fire.Skip)
	assert.Equal(t, "own-load", r.a.Cursor.ID)
	assert.Nil(t, r.poll(t), "recorded once")
}

// Its event and output carry ids and numbers only: the load, the count and
// a fixed sentence, never a name or a document's text.
func TestReviewsWaitingEvent(t *testing.T) {
	at := time.Date(2026, 11, 16, 7, 0, 0, 0, time.UTC)
	for open, summary := range map[int64]string{
		214:                          "214 to review in its ontology's current load.",
		1214:                         "1,214 to review in its ontology's current load.",
		automation.MaxReviewsWaiting: "10,000 or more to review in its ontology's current load.",
	} {
		event, out := eventOf(automation.ReviewsWaiting{OntologyID: "ont1", AtLeast: 50},
			[]model.FoundryAutomationEnded{{Cursor: model.FoundryAutomationCursor{At: at, ID: "load1"}, Rows: &open}})
		assert.Equal(t, model.FoundryAutomationEvent{Kind: "reviews_waiting", At: at, LoadID: "load1", Count: open}, event)
		assert.Equal(t, automation.Output{Summary: summary, Count: &open}, out)
		raw, err := json.Marshal(event)
		require.NoError(t, err)
		var fields map[string]any
		require.NoError(t, json.Unmarshal(raw, &fields))
		assert.Len(t, fields, 4, "kind, at, load_id and count: %s", raw)
	}
}

// A Founder step after it may only prepare a settle_review: the action waits
// for its owner, counted pending, and the run never settles a review.
func TestReviewsWaitingFounderStepOnlyPreparesASettle(t *testing.T) {
	f := newFixture(t)
	raw := spec(`[{"from":"start","to":"ask"}]`,
		node("start", "trigger", `{"kind":"reviews_waiting","ontology_id":"ont1","at_least":1}`),
		node("ask", "assistant", `{"instruction":"Settle what you are sure of."}`))
	run := f.queue(raw)
	open := int64(3)
	_, out := eventOf(automation.ReviewsWaiting{OntologyID: "ont1", AtLeast: 1},
		[]model.FoundryAutomationEnded{{Cursor: model.FoundryAutomationCursor{At: f.task.now(), ID: "load1"}, Rows: &open}})
	trigger, err := json.Marshal(out)
	require.NoError(t, err)
	f.db.nodes[run.ID]["start"].Output = trigger
	f.task.advance(context.Background(), run)
	require.Equal(t, model.FoundryAutomationNodeRunning, f.status(run.ID, "ask"))

	turn := f.db.endTurn(t, model.FoundryAgentTurnDone, agent.Reported{Summary: "Prepared one."})
	settle := &model.FoundryAgentAction{TurnID: turn.ID, Operation: "settle_review", Status: model.FoundryAgentActionPrepared}
	f.db.actions = []*model.FoundryAgentAction{settle}
	f.task.advance(context.Background(), run)
	assert.Equal(t, 1, f.output(t, run.ID, "ask").Pending, "it waits for its owner")
	assert.Equal(t, model.FoundryAgentActionPrepared, settle.Status, "nothing settled it")
	assert.Equal(t, model.FoundryAutomationRunSucceeded, f.db.finished[run.ID].Status)
}
