package foundrynotifyworker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/store"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

var now = time.Date(2026, time.October, 29, 12, 0, 0, 0, time.UTC)

// outboxStore is the store as the worker reads it: the emails it hands out
// in order, the reads they need, and what each claim ended with.
type outboxStore struct {
	mu      sync.Mutex
	pending []*model.FoundryNotification
	ended   map[string]model.FoundryNotificationOutcome
	// discovered are the windows each discovery was given.
	readySince         []time.Time
	followFrom         []time.Time
	followTo           []time.Time
	discoverErr        error
	claimErr           error
	finishErr          error
	readErr            error
	deleting, gone     map[string]bool
	ontologies         map[string]*model.FoundryOntology
	generations        map[string]*model.OntologyGeneration
	jobs               map[string]*model.FoundryGraphJob
	loads              map[string]*model.FoundryLoad
	discoverReady      int
	discoverFollowUps  int
	lastDiscoverLimits []int
}

func newOutbox(pending ...*model.FoundryNotification) *outboxStore {
	return &outboxStore{
		pending: pending, ended: map[string]model.FoundryNotificationOutcome{}, deleting: map[string]bool{}, gone: map[string]bool{},
		ontologies: map[string]*model.FoundryOntology{}, generations: map[string]*model.OntologyGeneration{},
		jobs: map[string]*model.FoundryGraphJob{}, loads: map[string]*model.FoundryLoad{},
	}
}

func (b *outboxStore) DiscoverFoundryOntologyReady(_ context.Context, since time.Time, limit int, newID func() string) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if newID() == "" {
		panic("no ids")
	}
	b.readySince, b.lastDiscoverLimits = append(b.readySince, since), append(b.lastDiscoverLimits, limit)
	return b.discoverReady, b.discoverErr
}

func (b *outboxStore) DiscoverFoundrySampleFollowUps(_ context.Context, from, to time.Time, limit int, _ func() string) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.followFrom, b.followTo, b.lastDiscoverLimits = append(b.followFrom, from), append(b.followTo, to), append(b.lastDiscoverLimits, limit)
	return b.discoverFollowUps, b.discoverErr
}

func (b *outboxStore) ClaimFoundryNotification(_ context.Context, token string, _ time.Duration) (*model.FoundryNotification, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.claimErr != nil {
		return nil, b.claimErr
	}
	if len(b.pending) == 0 {
		return nil, nil
	}
	n := b.pending[0]
	b.pending = b.pending[1:]
	n.Attempts++
	n.ClaimToken = &token
	return n, nil
}

func (b *outboxStore) FinishFoundryNotification(_ context.Context, id, token string, outcome model.FoundryNotificationOutcome) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if token == "" {
		panic("no token")
	}
	b.ended[id] = outcome
	return b.finishErr
}

func (b *outboxStore) WorkspaceDeleting(_ context.Context, id string) (bool, error) {
	switch {
	case b.readErr != nil:
		return false, b.readErr
	case b.gone[id]:
		return false, store.ErrNotFound
	}
	return b.deleting[id], nil
}

func (b *outboxStore) GetFoundryOntology(_ context.Context, workspaceID, id string) (*model.FoundryOntology, error) {
	if o, ok := b.ontologies[id]; ok && o.WorkspaceID == workspaceID {
		return o, nil
	}
	return nil, store.ErrNotFound
}

func (b *outboxStore) GetOntologyGeneration(_ context.Context, ontologyID, id string) (*model.OntologyGeneration, error) {
	if g, ok := b.generations[id]; ok && g.OntologyID == ontologyID {
		return g, nil
	}
	return nil, store.ErrNotFound
}

func (b *outboxStore) GetFoundryGraphJobByClientToken(_ context.Context, ontologyID, token string) (*model.FoundryGraphJob, error) {
	if j, ok := b.jobs[token]; ok && j.OntologyID == ontologyID {
		return j, nil
	}
	return nil, store.ErrNotFound
}

func (b *outboxStore) GetFoundryLoad(_ context.Context, ontologyID, id string) (*model.FoundryLoad, error) {
	if l, ok := b.loads[id]; ok && l.OntologyID == ontologyID {
		return l, nil
	}
	return nil, store.ErrNotFound
}

// mailbox is the host's mail: where each person is, and what was sent.
type mailbox struct {
	mu         sync.Mutex
	people     map[string]ports.Recipient
	recipErr   error
	sendErr    error
	sent       []sentEmail
	recipients int
}

type sentEmail struct {
	to    ports.Recipient
	email ports.Email
}

func (m *mailbox) Recipient(_ context.Context, userID string) (ports.Recipient, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recipients++
	if m.recipErr != nil {
		return ports.Recipient{}, m.recipErr
	}
	r, ok := m.people[userID]
	if !ok {
		return ports.Recipient{}, store.ErrNotFound
	}
	return r, nil
}

func (m *mailbox) Send(_ context.Context, to ports.Recipient, e ports.Email) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sendErr != nil {
		return m.sendErr
	}
	m.sent = append(m.sent, sentEmail{to, e})
	return nil
}

const siteURL = "https://foundry.example.com"

func newTask(box *outboxStore, mail *mailbox) *Task {
	return &Task{
		cfg: config.NotifyConfig{SiteURL: siteURL, PollInterval: time.Second, FollowUpAfter: 24 * time.Hour, FollowUpWindow: 72 * time.Hour,
			MaxAttempts: 3},
		box: box, mail: mail, site: site(siteURL), now: func() time.Time { return now },
	}
}

func ada() *mailbox {
	return &mailbox{people: map[string]ports.Recipient{
		"user-ada": {Email: "ada@example.com", FirstName: "Ada"}, "user-anon": {Email: "anon@example.com"},
	}}
}

// firstDraft is the store holding ontology ont-1 of ws-1, named name, and
// its first draft gen-1, asked for by user-ada, ended with status; and its
// email, pending.
func firstDraft(name string, status model.OntologyGenerationStatus) (*outboxStore, *model.FoundryNotification) {
	ontologyID := "ont-1"
	n := &model.FoundryNotification{ID: "n-1", Kind: model.FoundryNotifyOntologyReady, SubjectID: "gen-1", WorkspaceID: "ws-1", UserID: "user-ada",
		OntologyID: &ontologyID, Status: model.FoundryNotificationPending}
	box := newOutbox(n)
	box.ontologies["ont-1"] = &model.FoundryOntology{ID: "ont-1", WorkspaceID: "ws-1", Name: name}
	box.generations["gen-1"] = &model.OntologyGeneration{ID: "gen-1", WorkspaceID: "ws-1", OntologyID: "ont-1", Status: status}
	return box, n
}

// loaded adds gen-1's load job, whose load ended as status, failing with
// failure when it is not "".
func (b *outboxStore) loaded(status model.FoundryLoadStatus, failure string) {
	job := &model.FoundryGraphJob{ID: "load-1", LoadID: "load-1", OntologyID: "ont-1", Kind: model.FoundryGraphJobLoad}
	if failure != "" {
		job.Error = &failure
	}
	b.jobs["first-draft:gen-1"] = job
	b.loads["load-1"] = &model.FoundryLoad{ID: "load-1", OntologyID: "ont-1", Status: status}
}

const (
	ask      = siteURL + "/ask/ont-1?workspace=ws-1"
	overview = siteURL + "/ontology/ont-1?workspace=ws-1"
)

func TestNew(t *testing.T) {
	cfg := config.Defaults()
	cfg.Foundry.Notify.SiteURL = siteURL
	if _, err := New(Deps{Cfg: &cfg, Mail: ada()}); err == nil || !strings.Contains(err.Error(), "db") {
		t.Fatalf("without a store: %v", err)
	}
	if _, err := New(Deps{Cfg: &cfg, DB: store.Unimplemented{}}); err == nil || !strings.Contains(err.Error(), "ports.Mail") {
		t.Fatalf("without mail: %v", err)
	}
	for _, bad := range []string{"", "https://foundry.example.com/app", "ftp://foundry.example.com"} {
		cfg.Foundry.Notify.SiteURL = bad
		if _, err := New(Deps{Cfg: &cfg, DB: store.Unimplemented{}, Mail: ada()}); err == nil || !strings.Contains(err.Error(), "site_url") {
			t.Fatalf("site_url %q: %v", bad, err)
		}
	}
	cfg.Foundry.Notify.SiteURL = siteURL
	task, err := New(Deps{Cfg: &cfg, DB: store.Unimplemented{}, Mail: ada()})
	if err != nil {
		t.Fatal(err)
	}
	if task.Name() != "foundry-notify-worker" || task.site != siteURL || task.cfg.MaxAttempts != 5 {
		t.Fatalf("task = %+v", task)
	}
}

// Each way a first draft ends has its email, linking to the ontology, with
// the signature; only the follow-up has a topic.
func TestFoundryNotifyWorkerEmails(t *testing.T) {
	for name, c := range map[string]struct {
		setup   func() (*outboxStore, *model.FoundryNotification)
		subject string
		text    []string
		links   []string
		topic   string
	}{
		"a ready load": {
			setup: func() (*outboxStore, *model.FoundryNotification) {
				box, n := firstDraft("Billing", model.OntologyGenerationSucceeded)
				box.loaded(model.FoundryLoadReady, "")
				return box, n
			},
			subject: "Billing is ready to ask",
			text: []string{
				"Billing is loaded and ready. Foundry drafted it from your data, made it current and loaded it into its Helix database.",
				"Ask it a question: " + ask, "See what it holds: " + overview,
			},
			links: []string{ask, overview},
		},
		"a partial load": {
			setup: func() (*outboxStore, *model.FoundryNotification) {
				box, n := firstDraft("Billing", model.OntologyGenerationSucceeded)
				box.loaded(model.FoundryLoadPartial, "")
				return box, n
			},
			subject: "Billing is ready to ask",
			text: []string{
				"Billing is loaded, though the load stopped at this month's budget, so some of its documents weren't read.",
				"Ask it a question: " + ask, "See the load: " + overview,
			},
			links: []string{ask, overview},
		},
		"a failed load": {
			setup: func() (*outboxStore, *model.FoundryNotification) {
				box, n := firstDraft("Billing", model.OntologyGenerationSucceeded)
				box.loaded(model.FoundryLoadFailed, "the database refused the load; Helix will retry")
				return box, n
			},
			subject: "Billing couldn't be loaded",
			text: []string{
				"Foundry drafted Billing and made it current, but its load failed: The database refused the load; Helix will retry.",
				"Open it to try again: " + overview,
			},
			links: []string{overview},
		},
		"a failed load with no error": {
			setup: func() (*outboxStore, *model.FoundryNotification) {
				box, n := firstDraft("Billing", model.OntologyGenerationSucceeded)
				box.loaded(model.FoundryLoadFailed, "")
				return box, n
			},
			subject: "Billing couldn't be loaded",
			text:    []string{"Foundry drafted Billing and made it current, but its load failed.\n\nOpen it to try again: " + overview},
			links:   []string{overview},
		},
		"a failed draft": {
			setup: func() (*outboxStore, *model.FoundryNotification) {
				box, n := firstDraft("Billing", model.OntologyGenerationFailed)
				failure := "the generated ontology is not valid (gave up after 3 attempts)"
				box.generations["gen-1"].Error = &failure
				return box, n
			},
			subject: "Billing couldn't be drafted",
			text: []string{
				"Foundry's agent couldn't draft Billing: The generated ontology is not valid (gave up after 3 attempts).",
				"Open it to try again: " + overview,
			},
			links: []string{overview},
		},
		"a draft left for review": {
			setup: func() (*outboxStore, *model.FoundryNotification) {
				return firstDraft("Billing", model.OntologyGenerationSucceeded)
			},
			subject: "Billing's first draft is ready",
			text:    []string{"Foundry's agent drafted Billing. Review the draft and make it current, then load it: " + overview},
			links:   []string{overview},
		},
		"an ontology with no name": {
			setup: func() (*outboxStore, *model.FoundryNotification) {
				box, n := firstDraft(" \n ", model.OntologyGenerationSucceeded)
				box.loaded(model.FoundryLoadFailed, "")
				return box, n
			},
			subject: "Your ontology couldn't be loaded",
			text:    []string{"Foundry drafted your ontology and made it current, but its load failed."},
			links:   []string{overview},
		},
		"the sample's follow-up": {
			setup: func() (*outboxStore, *model.FoundryNotification) {
				n := &model.FoundryNotification{ID: "n-1", Kind: model.FoundryNotifySampleFollowUp, SubjectID: "user-ada", WorkspaceID: "ws-1",
					UserID: "user-ada", Status: model.FoundryNotificationPending}
				return newOutbox(n), n
			},
			subject: "Try Foundry on your own data",
			text: []string{
				"Yesterday you watched Foundry turn Acme Corp's database and documents into an ontology you could ask questions of. " +
					"It does the same with yours.",
				"Connect a database or an app, or upload documents, and say what the ontology should serve. " +
					"Foundry drafts it, loads it and emails you when it's ready, in a few minutes.",
				"Connect your data: " + siteURL + "/ontology/new?workspace=ws-1",
				"If you get stuck, reply to this email and I'll help.",
			},
			links: []string{siteURL + "/ontology/new?workspace=ws-1"},
			topic: ports.TopicFoundryTips,
		},
	} {
		t.Run(name, func(t *testing.T) {
			box, n := c.setup()
			mail := ada()
			newTask(box, mail).tick(context.Background())
			if _, ok := box.ended[n.ID].(model.FoundryNotificationDelivered); !ok || len(mail.sent) != 1 {
				t.Fatalf("ended %#v after sending %d", box.ended[n.ID], len(mail.sent))
			}
			got := mail.sent[0]
			e := got.email
			if got.to.Email != "ada@example.com" || e.Subject != c.subject || e.Topic != c.topic {
				t.Fatalf("sent %q to %+v under topic %q", e.Subject, got.to, e.Topic)
			}
			if !strings.HasPrefix(e.Text, "Hi Ada,\n\n") || !strings.HasSuffix(e.Text, "\n\nXav\nHelix Foundry\n") {
				t.Fatalf("text = %q", e.Text)
			}
			for _, want := range c.text {
				if !strings.Contains(e.Text, want) {
					t.Errorf("text lacks %q:\n%s", want, e.Text)
				}
			}
			if !strings.Contains(e.HTML, "<p style=\"margin:0 0 16px;\">Hi Ada,</p>") || !strings.Contains(e.HTML, ">Xav<br>Helix Foundry</p>") {
				t.Errorf("html lacks the greeting or the signature:\n%s", e.HTML)
			}
			for _, link := range c.links {
				if !strings.Contains(e.HTML, `<a href="`+link+`"`) {
					t.Errorf("html lacks a link to %s:\n%s", link, e.HTML)
				}
			}
			if strings.Count(e.HTML, "<a ") != len(c.links) || strings.Count(e.Text, "https://") != len(c.links) {
				t.Errorf("links: text %d, html %d, want %d", strings.Count(e.Text, "https://"), strings.Count(e.HTML, "<a "), len(c.links))
			}
		})
	}
}

// A name is the user's to write: in HTML it is escaped, in the subject and
// text it is as written, on one line.
func TestFoundryNotifyWorkerEscapes(t *testing.T) {
	box, n := firstDraft("<b>Acme</b> & \"co\"\r\nBcc: x@example.com", model.OntologyGenerationSucceeded)
	box.ontologies["ont/1 ?"] = box.ontologies["ont-1"]
	box.ontologies["ont/1 ?"].ID = "ont/1 ?"
	box.generations["gen-1"].OntologyID = "ont/1 ?"
	box.loaded(model.FoundryLoadReady, "")
	box.jobs["first-draft:gen-1"].OntologyID = "ont/1 ?"
	box.loads["load-1"].OntologyID = "ont/1 ?"
	id := "ont/1 ?"
	n.OntologyID, n.WorkspaceID = &id, "ws&1"
	box.ontologies["ont/1 ?"].WorkspaceID = "ws&1"
	mail := ada()
	newTask(box, mail).tick(context.Background())
	if len(mail.sent) != 1 {
		t.Fatalf("ended %#v", box.ended[n.ID])
	}
	e := mail.sent[0].email
	if e.Subject != `<b>Acme</b> & "co" Bcc: x@example.com is ready to ask` {
		t.Fatalf("subject = %q", e.Subject)
	}
	if strings.Contains(e.HTML, "<b>") || !strings.Contains(e.HTML, "&lt;b&gt;Acme&lt;/b&gt; &amp; &#34;co&#34;") {
		t.Fatalf("html does not escape the name:\n%s", e.HTML)
	}
	escaped := siteURL + "/ask/ont%2F1%20%3F?workspace=ws%261"
	if !strings.Contains(e.Text, "Ask it a question: "+escaped+"\n") || !strings.Contains(e.HTML, `href="`+escaped+`"`) {
		t.Fatalf("links do not escape the ids:\n%s\n%s", e.Text, e.HTML)
	}
}

// Without a first name the greeting names no one.
func TestFoundryNotifyWorkerGreeting(t *testing.T) {
	box, n := firstDraft("Billing", model.OntologyGenerationSucceeded)
	n.UserID = "user-anon"
	mail := ada()
	newTask(box, mail).tick(context.Background())
	if len(mail.sent) != 1 || !strings.HasPrefix(mail.sent[0].email.Text, "Hi,\n\n") || !strings.Contains(mail.sent[0].email.HTML, ">Hi,</p>") {
		t.Fatalf("sent %+v", mail.sent)
	}
}

// An email whose person, workspace, ontology, generation or load is gone is
// skipped, unsent and for good.
func TestFoundryNotifyWorkerSkips(t *testing.T) {
	for name, c := range map[string]struct {
		change func(*outboxStore, *model.FoundryNotification)
		why    string
	}{
		"a person with no address":  {func(_ *outboxStore, n *model.FoundryNotification) { n.UserID = "user-gone" }, "the person is gone"},
		"a workspace being deleted": {func(b *outboxStore, _ *model.FoundryNotification) { b.deleting["ws-1"] = true }, "the workspace is gone"},
		"a workspace gone":          {func(b *outboxStore, _ *model.FoundryNotification) { b.gone["ws-1"] = true }, "the workspace is gone"},
		"a deleted ontology": {func(b *outboxStore, _ *model.FoundryNotification) {
			b.ontologies["ont-1"].DeletedAt = &now
		}, "the ontology is gone"},
		"an ontology gone":  {func(b *outboxStore, _ *model.FoundryNotification) { delete(b.ontologies, "ont-1") }, "the ontology is gone"},
		"a generation gone": {func(b *outboxStore, _ *model.FoundryNotification) { delete(b.generations, "gen-1") }, "the generation is gone"},
		"a load deleted": {func(b *outboxStore, _ *model.FoundryNotification) {
			b.loads["load-1"].Status = model.FoundryLoadDeleted
		}, "load is deleted"},
		"a load gone":             {func(b *outboxStore, _ *model.FoundryNotification) { delete(b.loads, "load-1") }, "load is gone"},
		"an email of no ontology": {func(_ *outboxStore, n *model.FoundryNotification) { n.OntologyID = nil }, "names no ontology"},
		"a generation still running": {func(b *outboxStore, _ *model.FoundryNotification) {
			b.generations["gen-1"].Status = model.OntologyGenerationRunning
		}, "not ended"},
		"a kind of no email": {func(_ *outboxStore, n *model.FoundryNotification) { n.Kind = "digest" }, "no email of kind"},
	} {
		t.Run(name, func(t *testing.T) {
			box, n := firstDraft("Billing", model.OntologyGenerationSucceeded)
			box.loaded(model.FoundryLoadReady, "")
			c.change(box, n)
			mail := ada()
			newTask(box, mail).tick(context.Background())
			skip, ok := box.ended["n-1"].(model.FoundryNotificationSkip)
			if !ok || !strings.Contains(skip.Reason, c.why) || len(mail.sent) != 0 {
				t.Fatalf("ended %#v after sending %d, want skipped for %q", box.ended["n-1"], len(mail.sent), c.why)
			}
		})
	}
}

// A send that fails is tried again later, waiting longer each time, and
// fails for good on the last attempt; so do reads that fail.
func TestFoundryNotifyWorkerRetries(t *testing.T) {
	for attempts, want := range map[int]time.Duration{1: time.Minute, 2: 2 * time.Minute} {
		box, n := firstDraft("Billing", model.OntologyGenerationSucceeded)
		n.Attempts = attempts - 1
		mail := ada()
		mail.sendErr = errors.New("SES throttled")
		newTask(box, mail).tick(context.Background())
		retry, ok := box.ended["n-1"].(model.FoundryNotificationRetry)
		if !ok || !retry.At.Equal(now.Add(want)) || !strings.Contains(retry.Error, "SES throttled") {
			t.Fatalf("attempt %d ended %#v, want a retry in %s", attempts, box.ended["n-1"], want)
		}
	}
	box, n := firstDraft("Billing", model.OntologyGenerationSucceeded)
	n.Attempts = 2
	mail := ada()
	mail.sendErr = errors.New("SES throttled")
	newTask(box, mail).tick(context.Background())
	if give, ok := box.ended["n-1"].(model.FoundryNotificationGiveUp); !ok || !strings.Contains(give.Error, "gave up after 3 attempts") {
		t.Fatalf("the last attempt ended %#v", box.ended["n-1"])
	}
	for name, change := range map[string]func(*outboxStore, *mailbox){
		"the store":            func(b *outboxStore, _ *mailbox) { b.readErr = errors.New("connection reset") },
		"the person's address": func(_ *outboxStore, m *mailbox) { m.recipErr = errors.New("directory down") },
	} {
		box, _ := firstDraft("Billing", model.OntologyGenerationSucceeded)
		mail := ada()
		change(box, mail)
		newTask(box, mail).tick(context.Background())
		if _, ok := box.ended["n-1"].(model.FoundryNotificationRetry); !ok || len(mail.sent) != 0 {
			t.Fatalf("%s failing: ended %#v", name, box.ended["n-1"])
		}
	}
	// A claim past max_attempts is one whose workers kept stopping while it
	// was sent: it fails, and is never sent again.
	box, n = firstDraft("Billing", model.OntologyGenerationSucceeded)
	n.Attempts = 3
	mail = ada()
	newTask(box, mail).tick(context.Background())
	if _, ok := box.ended["n-1"].(model.FoundryNotificationGiveUp); !ok || len(mail.sent) != 0 || mail.recipients != 0 {
		t.Fatalf("a claim past max_attempts ended %#v after sending %d", box.ended["n-1"], len(mail.sent))
	}
}

// Each tick looks for emails in its windows, then sends every one due.
func TestFoundryNotifyWorkerTick(t *testing.T) {
	box, first := firstDraft("Billing", model.OntologyGenerationSucceeded)
	second := &model.FoundryNotification{ID: "n-2", Kind: model.FoundryNotifySampleFollowUp, SubjectID: "user-ada", WorkspaceID: "ws-1",
		UserID: "user-ada", Status: model.FoundryNotificationPending}
	box.pending = append(box.pending, second)
	mail := ada()
	newTask(box, mail).tick(context.Background())
	if len(box.readySince) != 1 || !box.readySince[0].Equal(now.Add(-72*time.Hour)) ||
		len(box.followFrom) != 1 || !box.followFrom[0].Equal(now.Add(-72*time.Hour)) || !box.followTo[0].Equal(now.Add(-24*time.Hour)) {
		t.Fatalf("windows: ready since %v, follow-ups from %v to %v", box.readySince, box.followFrom, box.followTo)
	}
	for _, limit := range box.lastDiscoverLimits {
		if limit != discoverBatch {
			t.Fatalf("limits = %v", box.lastDiscoverLimits)
		}
	}
	if len(box.ended) != 2 || len(mail.sent) != 2 || box.ended[first.ID] == nil || box.ended[second.ID] == nil {
		t.Fatalf("ended %v, sent %d", box.ended, len(mail.sent))
	}

	// Discovery failing doesn't stop the sending; a claim failing ends the tick.
	box, _ = firstDraft("Billing", model.OntologyGenerationSucceeded)
	box.discoverErr = errors.New("connection reset")
	mail = ada()
	newTask(box, mail).tick(context.Background())
	if len(mail.sent) != 1 {
		t.Fatalf("sent %d after discovery failed", len(mail.sent))
	}
	box, _ = firstDraft("Billing", model.OntologyGenerationSucceeded)
	box.claimErr = errors.New("connection reset")
	mail = ada()
	newTask(box, mail).tick(context.Background())
	if len(mail.sent) != 0 || len(box.ended) != 0 {
		t.Fatalf("sent %d after the claim failed", len(mail.sent))
	}
	// A claim lost while sending, or an end that can't be recorded, leaves
	// the email to the store: the tick goes on.
	for _, err := range []error{store.ErrNotFound, errors.New("connection reset")} {
		box, _ = firstDraft("Billing", model.OntologyGenerationSucceeded)
		box.finishErr = err
		box.pending = append(box.pending, second)
		mail = ada()
		newTask(box, mail).tick(context.Background())
		if len(mail.sent) != 2 {
			t.Fatalf("finish failing with %v: sent %d", err, len(mail.sent))
		}
	}
	// A stopping worker claims nothing more.
	box, _ = firstDraft("Billing", model.OntologyGenerationSucceeded)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mail = ada()
	newTask(box, mail).tick(ctx)
	if len(mail.sent) != 0 {
		t.Fatalf("sent %d while stopping", len(mail.sent))
	}
}

func TestSentence(t *testing.T) {
	for in, want := range map[string]string{
		"":                       "",
		"  ":                     "",
		"the load failed":        "The load failed.",
		"the load failed.":       "The load failed.",
		"why?":                   "Why?",
		"élan lost":              "Élan lost.",
		" Helix will retry; ok ": "Helix will retry; ok.",
	} {
		if got := sentence(in); got != want {
			t.Errorf("sentence(%q) = %q, want %q", in, got, want)
		}
	}
}
