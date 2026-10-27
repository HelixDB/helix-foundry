package enrichment

import (
	"fmt"
	"iter"
	"strings"
	"time"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
)

// Clue is one entity of the enriched type as the engine's Clues sends it
// (ontologyagent.ReadClues): its entity, its name (the name guard's input)
// and its normalised clues, which its profile repeats as clue_name,
// clue_email and clue_linkedin_url to bind it. The engine has already
// dropped a value two entities share, a clue with two values and a value
// its kind rejects; an entity may have no name or no clue left.
type Clue = ontologyagent.CluedEntity

// Action is what a load does with one entity.
type Action string

const (
	// ActionCall looks the entity up.
	ActionCall Action = "call"
	// ActionCarry reuses an earlier load's result, at no cost.
	ActionCarry Action = "carry"
	// ActionSkip leaves the entity out: no full name or no usable clue.
	ActionSkip Action = "skip"
)

// Reasons the planner drops a clue or skips an entity, in the filters' order.
const (
	ReasonNoFullName  = "no_full_name"
	ReasonBadLinkedIn = "bad_linkedin"
	ReasonBadEmail    = "bad_email"
	ReasonFreeMail    = "free_mail"
	ReasonRoleMailbox = "role_mailbox"
	ReasonSharedSlug  = "shared_slug"
	ReasonNoClue      = "no_clue"
)

// Sent are the clues a lookup sends: an email, a LinkedIn slug, or both.
type Sent struct {
	Email string `json:"email,omitempty"`
	Slug  string `json:"slug,omitempty"`
}

// Entry is one line of plan.jsonl.gz: an entity, what this load sends for
// it, and what it does with it. Reason says why a clue was dropped or the
// entity skipped. A carried entry holds the result it carries, already
// checked against this load's values.
type Entry struct {
	Clue
	Sent    Sent    `json:"sent"`
	Action  Action  `json:"action"`
	Reason  string  `json:"reason,omitempty"`
	Carried *Result `json:"carried,omitempty"`
}

// Result is one line of results.jsonl.gz: everyone a load called or carried.
// Accepted and held results keep their full row, which later loads carry.
// CheckedAt is when the provider was asked.
type Result struct {
	Sent      Sent      `json:"sent"`
	Outcome   Outcome   `json:"outcome"`
	Reason    string    `json:"reason,omitempty"`
	Score     *int      `json:"score,omitempty"`
	ProfileID string    `json:"profile_id,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
	Row       *Profile  `json:"row,omitempty"`
	// Partial is context.dev saying its answer was unfinished, fields left
	// out: an accepted result is carried as any other, a held or not_found
	// one is looked up again.
	Partial bool `json:"partial,omitempty"`
}

// PlanCounts are what planning found.
type PlanCounts struct {
	// People are every entity Clues sent.
	People int64
	// NoName have fewer than two letter tokens in their name.
	NoName int64
	// NoClue had every clue dropped.
	NoClue int64
	// Shared lost their slug to another entity's.
	Shared  int64
	Carried int64
	Call    int64
}

// Planner plans a load's lookups.
type Planner struct {
	// MinScore is the accept gate's least score.
	MinScore int
	// NotFoundFor is how long a person not found is not asked about again.
	NotFoundFor time.Duration
	Now         time.Time
}

// Plan decides, for each of people, what the load sends and whether it
// calls, carries an earlier result or skips, and hands each entry to emit
// once. The filters run in order:
//  1. no_full_name: fewer than two letter tokens in the name skips it;
//  2. bad_linkedin: a URL that is not a profile drops the slug;
//  3. free_mail, then 4. role_mailbox: drop the email (bad_email, one with
//     no domain);
//  5. shared_slug: a slug two entities share is dropped for both.
//
// An entity with no clue left is skipped. Then previous, the ontology's
// newest results, read one at a time, are carried forward where they still
// hold (see carry); everyone else is called. Memory holds no one's clues
// and no result's row: an earlier result is checked as it is read against
// the people it names, read back from people's file, and an entity it
// carries for good is emitted at once; the rest follow in entity order.
// people are planned once, and hold no memory after.
func (p Planner) Plan(people *People, previous iter.Seq2[Result, error], emit func(Entry) error) (PlanCounts, error) {
	if err := people.settle(); err != nil {
		return PlanCounts{}, err
	}
	defer people.forget()
	counts := people.counts
	// notFound[i] is where the not_found result person i carries unless a
	// later result is carried for good starts, plus one.
	var notFound []int64
	for r, err := range previous {
		if err != nil {
			return counts, err
		}
		err := people.match(r.Sent, func(person uint32, e Entry) error {
			carried, final := p.carry(e, r)
			switch {
			case carried == nil:
			case final:
				e.Action, e.Carried = ActionCarry, carried
				people.state[person] |= carriedFinal
				if notFound != nil {
					notFound[person] = 0
				}
				counts.Carried++
				return emit(e)
			default:
				at, err := people.keep(*carried)
				if err != nil {
					return err
				}
				if notFound == nil {
					notFound = make([]int64, len(people.at))
				}
				notFound[person] = at + 1
			}
			return nil
		})
		if err != nil {
			return counts, err
		}
	}
	if err := people.w.Flush(); err != nil {
		return counts, fmt.Errorf("write carried results: %w", err)
	}
	err := people.each(func(person uint32, e Entry) error {
		switch {
		case people.state[person]&carriedFinal != 0:
			return nil
		case notFound != nil && notFound[person] != 0:
			r, err := people.kept(notFound[person] - 1)
			if err != nil {
				return err
			}
			e.Action, e.Carried = ActionCarry, &r
			counts.Carried++
		case e.Action == ActionCall:
			counts.Call++
		}
		return emit(e)
	})
	return counts, err
}

// filter is the entry of an entity before filter 5, which needs everyone:
// the filters Plan runs on each entity alone.
func filter(c Clue) Entry {
	e := Entry{Clue: c, Action: ActionCall}
	if len(NameTokens(c.Name)) < 2 {
		e.Action, e.Reason = ActionSkip, ReasonNoFullName
		return e
	}
	if c.LinkedInURL != "" {
		if slug, ok := CanonicalLinkedIn(c.LinkedInURL); ok {
			e.Sent.Slug = slug
		} else {
			e.Reason = ReasonBadLinkedIn
		}
	}
	if c.Email != "" {
		local, domain, ok := strings.Cut(strings.ToLower(c.Email), "@")
		switch {
		case !ok || local == "" || strings.Contains(domain, "@"):
			e.Reason = ReasonBadEmail
		case FreeMail(domain):
			e.Reason = ReasonFreeMail
		case RoleMailbox(local):
			e.Reason = ReasonRoleMailbox
		default:
			e.Sent.Email = local + "@" + domain
		}
	}
	return e
}

// carry returns the result e reuses of r, an earlier result found by a clue
// e sends now, checked against this load's values; nil when it doesn't
// hold. final is false for a not_found, which an accepted or held result
// read later still replaces:
//   - an accepted or held result with a row, whose profile has e's slug if e
//     sends one: the gate runs again on the row (free), so a renamed person
//     or a changed email is held without a call, and a held row that now
//     passes is accepted. It stands for e's current clues, and its row takes
//     e's clue values and name;
//   - a not_found result checked within NotFoundFor, whose clues include all
//     e sends now: it is carried as it was.
//
// Uncertain and skipped results, held and not_found results of a partial
// answer, and people never reached, are looked up.
func (p Planner) carry(e Entry, r Result) (carried *Result, final bool) {
	switch {
	case r.Partial && r.Outcome != Accepted:
		return nil, false
	case (r.Outcome == Accepted || r.Outcome == Held) && r.Row != nil && r.Score != nil:
		if e.Sent.Slug != "" {
			if slug, ok := CanonicalLinkedIn(r.Row.LinkedInURL); !ok || slug != e.Sent.Slug {
				return nil, false
			}
		}
		row := *r.Row
		row.ClueEmail, row.ClueLinkedInURL, row.ClueName = e.Email, e.LinkedInURL, e.Name
		outcome, reason := Gate(&row, *r.Score, p.MinScore, e.Name, e.Sent)
		return &Result{
			Sent: e.Sent, Outcome: outcome, Reason: reason, Score: r.Score, ProfileID: r.ProfileID, CheckedAt: r.CheckedAt, Row: &row,
			Partial: r.Partial,
		}, true
	case r.Outcome == NotFound:
		subset := (e.Sent.Email == "" || e.Sent.Email == r.Sent.Email) && (e.Sent.Slug == "" || e.Sent.Slug == r.Sent.Slug)
		if subset && p.Now.Sub(r.CheckedAt) < p.NotFoundFor {
			return &r, false
		}
	}
	return nil, false
}

// Fresh is one line of results/<b>.jsonl.gz: how this step's lookup of an
// entity ended. A batch's lines follow the plan's order of its calls.
type Fresh struct {
	Entity string `json:"entity"`
	Result
}

// Published counts what a publication holds.
type Published struct {
	// Fresh counts this step's own calls by outcome, duplicates held.
	Fresh map[Outcome]int64
	// Profiles is how many accepted rows the load applies.
	Profiles int64
}

// Publish reads plan twice and fresh twice (each call reads afresh), joining
// the plan's carried results with fresh, this step's results in the plan's
// order of its calls, and hands emit every result. An accepted profile on
// two or more entities is held (duplicate_profile): the name guard binds one
// profile to one person. Entities neither carried nor in fresh were never
// reached and are left out, so the next load looks them up. Only the
// profile ids are held in memory, never the rows. fresh out of the plan's
// order is an error, which names the result by position (errors are
// logged, and an entity's key may be an email).
func Publish(plan func() iter.Seq2[Entry, error], fresh func() iter.Seq2[Fresh, error], emit func(Result) error) (Published, error) {
	// join hands each, every result with whether it is fresh.
	join := func(each func(Result, bool) error) error {
		next, stop := iter.Pull2(fresh())
		defer stop()
		pending, err, more := next()
		joined := 0
		for e, planErr := range plan() {
			switch {
			case planErr != nil:
				return planErr
			case more && err != nil:
				return err
			case e.Action == ActionCarry && e.Carried != nil:
				if err := each(*e.Carried, false); err != nil {
					return err
				}
			case e.Action == ActionCall && more && pending.Entity == e.Entity:
				if err := each(pending.Result, true); err != nil {
					return err
				}
				pending, err, more = next()
				joined++
			}
		}
		switch {
		case more && err != nil:
			return err
		case more:
			return fmt.Errorf("fresh result %d is not in the plan's order of calls", joined)
		}
		return nil
	}
	people := map[string]int{}
	if err := join(func(r Result, _ bool) error {
		if r.Outcome == Accepted {
			people[r.ProfileID]++
		}
		return nil
	}); err != nil {
		return Published{}, err
	}
	pub := Published{Fresh: map[Outcome]int64{}}
	err := join(func(r Result, isFresh bool) error {
		if r.Outcome == Accepted && people[r.ProfileID] > 1 {
			r.Outcome, r.Reason = Held, ReasonDuplicateProfile
		}
		if r.Outcome == Accepted {
			pub.Profiles++
		}
		if isFresh {
			pub.Fresh[r.Outcome]++
		}
		return emit(r)
	})
	if err != nil {
		return Published{}, err
	}
	return pub, nil
}
