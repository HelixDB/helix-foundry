package enrichment

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
)

var planNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

var planner = Planner{MinScore: 90, NotFoundFor: 90 * 24 * time.Hour, Now: planNow}

// seq yields results as a file would.
func seq(results ...Result) iter.Seq2[Result, error] {
	return func(yield func(Result, error) bool) {
		for _, r := range results {
			if !yield(r, nil) {
				return
			}
		}
	}
}

// collide hashes every value alike: People must then tell values apart by
// reading them back.
func collide(string) uint64 { return 7 }

// planOf runs the planner on clues, each value hashed by hash (nil: People'
// own), and returns its entries as emitted.
func planOf(t *testing.T, hash func(string) uint64, clues []Clue, previous ...Result) ([]Entry, PlanCounts) {
	t.Helper()
	people, err := (&Files{Dir: t.TempDir()}).People(len(clues) + 1)
	if err != nil {
		t.Fatal(err)
	}
	defer people.Close()
	if hash != nil {
		people.hash = hash
	}
	for _, c := range clues {
		if err := people.Add(c); err != nil {
			t.Fatal(err)
		}
	}
	var out []Entry
	counts, err := planner.Plan(people, seq(previous...), func(e Entry) error { out = append(out, e); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return out, counts
}

// plan runs the planner and returns its entries by entity. It plans again
// with every value's hash colliding, which must change nothing.
func plan(t *testing.T, clues []Clue, previous ...Result) (map[string]Entry, PlanCounts) {
	t.Helper()
	entries, counts := planOf(t, nil, clues, previous...)
	collided, collidedCounts := planOf(t, collide, clues, previous...)
	if !reflect.DeepEqual(entries, collided) || counts != collidedCounts {
		t.Fatalf("colliding hashes changed the plan: %+v %+v, not %+v %+v", collided, collidedCounts, entries, counts)
	}
	out := map[string]Entry{}
	for _, e := range entries {
		if _, dup := out[e.Entity]; dup {
			t.Fatalf("entity %s emitted twice", e.Entity)
		}
		out[e.Entity] = e
	}
	if len(out) != len(clues) {
		t.Fatalf("%d entries for %d entities", len(out), len(clues))
	}
	return out, counts
}

func score(n int) *int { return &n }

// accepted is the result an earlier load kept for Ada, sent sent.
func accepted(sent Sent) Result {
	return Result{Sent: sent, Outcome: Accepted, Score: score(96), ProfileID: "li:ada-lovelace", CheckedAt: planNow.AddDate(0, -1, 0), Row: ada()}
}

func ExamplePlanner_Plan() {
	clues := []Clue{
		{Entity: "c_17", Name: "Ada Lovelace", Email: "ada@acme.io"},
		{Entity: "c_18", Name: "Grace Hopper", Email: "grace@gmail.com", LinkedInURL: "linkedin.com/in/grace-hopper"},
		{Entity: "c_19", Name: "Cher", Email: "cher@acme.io"},
	}
	people, err := (&Files{}).People(1000)
	if err != nil {
		panic(err)
	}
	defer people.Close()
	for _, c := range clues {
		if err := people.Add(c); err != nil {
			panic(err)
		}
	}
	p := Planner{MinScore: 90, NotFoundFor: 90 * 24 * time.Hour, Now: time.Now()}
	_, _ = p.Plan(people, func(func(Result, error) bool) {}, func(e Entry) error {
		fmt.Printf("%s %s %+v %q\n", e.Entity, e.Action, e.Sent, e.Reason)
		return nil
	})
	// Output:
	// c_17 call {Email:ada@acme.io Slug:} ""
	// c_18 call {Email: Slug:grace-hopper} "free_mail"
	// c_19 skip {Email: Slug:} "no_full_name"
}

func TestPlanFilters(t *testing.T) {
	// Each filter drops what it can't use and keeps the rest; an entity with
	// nothing left is skipped with the last reason, and the counts say so.
	t.Run("drops unusable clues in order", func(t *testing.T) {
		entries, counts := plan(t, []Clue{
			{Entity: "short", Name: "Ada", Email: "ada@acme.io"},
			{Entity: "badli", Name: "Ada Lovelace", Email: "ada@acme.io", LinkedInURL: "https://www.linkedin.com/pub/ada/1/2"},
			{Entity: "free", Name: "Grace Hopper", Email: "grace@gmail.com", LinkedInURL: "https://www.linkedin.com/in/grace-hopper"},
			{Entity: "role", Name: "Sales Team", Email: "sales@acme.io"},
			{Entity: "share1", Name: "Alan Turing", LinkedInURL: "https://www.linkedin.com/in/alan-turing"},
			{Entity: "share2", Name: "Alan Turing", Email: "alan@acme.io", LinkedInURL: "https://uk.linkedin.com/in/Alan-Turing/"},
			{Entity: "noemail", Name: "Edsger Dijkstra", Email: "edsger"},
			{Entity: "none", Name: "Barbara Liskov"},
		})
		want := map[string]Entry{
			"short":   {Action: ActionSkip, Reason: ReasonNoFullName},
			"badli":   {Action: ActionCall, Reason: ReasonBadLinkedIn, Sent: Sent{Email: "ada@acme.io"}},
			"free":    {Action: ActionCall, Reason: ReasonFreeMail, Sent: Sent{Slug: "grace-hopper"}},
			"role":    {Action: ActionSkip, Reason: ReasonRoleMailbox},
			"share1":  {Action: ActionSkip, Reason: ReasonSharedSlug},
			"share2":  {Action: ActionCall, Reason: ReasonSharedSlug, Sent: Sent{Email: "alan@acme.io"}},
			"noemail": {Action: ActionSkip, Reason: ReasonBadEmail},
			"none":    {Action: ActionSkip, Reason: ReasonNoClue},
		}
		for entity, w := range want {
			got := entries[entity]
			if got.Action != w.Action || got.Reason != w.Reason || got.Sent != w.Sent {
				t.Errorf("%s = %s %s %+v, want %s %s %+v", entity, got.Action, got.Reason, got.Sent, w.Action, w.Reason, w.Sent)
			}
		}
		if counts != (PlanCounts{People: 8, NoName: 1, NoClue: 4, Shared: 2, Call: 3}) {
			t.Fatalf("counts = %+v", counts)
		}
		if entries["free"].Email != "grace@gmail.com" {
			t.Fatal("an entity keeps its own clue values for the binding")
		}
	})

	// A file that can't be read stops the plan: no load plans from half the results.
	t.Run("stops at an unreadable earlier result", func(t *testing.T) {
		broken := func(yield func(Result, error) bool) { yield(Result{}, errors.New("truncated")) }
		people, err := (&Files{Dir: t.TempDir()}).People(1)
		if err != nil {
			t.Fatal(err)
		}
		defer people.Close()
		if err := people.Add(Clue{Entity: "a", Name: "Ada Lovelace", Email: "ada@acme.io"}); err != nil {
			t.Fatal(err)
		}
		if _, err := planner.Plan(people, broken, func(Entry) error { return nil }); err == nil {
			t.Fatal("planned past an unreadable file")
		}
	})
}

func TestPeople(t *testing.T) {
	files := &Files{Dir: t.TempDir()}

	// max bounds the people a step plans, so its memory: one more is refused.
	t.Run("refuses a person past max", func(t *testing.T) {
		people, err := files.People(2)
		if err != nil {
			t.Fatal(err)
		}
		defer people.Close()
		for i := range 2 {
			if err := people.Add(Clue{Entity: fmt.Sprint(i)}); err != nil {
				t.Fatal(err)
			}
		}
		if err := people.Add(Clue{Entity: "2"}); err == nil {
			t.Fatal("added a person past max")
		}
		for _, max := range []int{0, -1} {
			if _, err := files.People(max); err == nil {
				t.Fatalf("people of at most %d", max)
			}
		}
	})

	// People are planned once, and taken only before.
	t.Run("plans once", func(t *testing.T) {
		people, err := files.People(2)
		if err != nil {
			t.Fatal(err)
		}
		defer people.Close()
		emit := func(Entry) error { return nil }
		if _, err := planner.Plan(people, seq(), emit); err != nil {
			t.Fatal(err)
		}
		if _, err := planner.Plan(people, seq(), emit); err == nil {
			t.Fatal("planned twice")
		}
		if err := people.Add(Clue{Entity: "late"}); err == nil {
			t.Fatal("added after the plan")
		}
	})

	// emit's error ends the plan, whether it came carrying for good or in
	// entity order.
	t.Run("stops at emit's error", func(t *testing.T) {
		full := errors.New("disk full")
		for _, previous := range []iter.Seq2[Result, error]{seq(accepted(Sent{Email: "ada@acme.io"})), seq()} {
			people, err := files.People(1)
			if err != nil {
				t.Fatal(err)
			}
			if err := people.Add(Clue{Entity: "c_17", Name: "Ada Lovelace", Email: "ada@acme.io"}); err != nil {
				t.Fatal(err)
			}
			if _, err := planner.Plan(people, previous, func(Entry) error { return full }); !errors.Is(err, full) {
				t.Fatalf("err = %v", err)
			}
			people.Close()
		}
	})

	// A file that can't be written or read fails the plan, never a person.
	t.Run("fails on a file it can't use", func(t *testing.T) {
		people, err := files.People(2)
		if err != nil {
			t.Fatal(err)
		}
		if err := people.Add(Clue{Entity: "c_17", Name: "Ada Lovelace", Email: "ada@acme.io"}); err != nil {
			t.Fatal(err)
		}
		people.Close()
		if _, err := planner.Plan(people, seq(accepted(Sent{Email: "ada@acme.io"})), func(Entry) error { return nil }); err == nil {
			t.Fatal("planned from a closed file")
		}
	})
}

func TestCarryForward(t *testing.T) {
	adaClue := Clue{Entity: "c_17", Name: "Ada Lovelace", Email: "ada@acme.io"}

	// The profile found by email is the same one the new LinkedIn URL names:
	// adding the URL costs nothing.
	t.Run("an added LinkedIn URL makes no call", func(t *testing.T) {
		clue := adaClue
		clue.LinkedInURL = "https://www.linkedin.com/in/ada-lovelace/"
		entries, counts := plan(t, []Clue{clue}, accepted(Sent{Email: "ada@acme.io"}))
		e := entries["c_17"]
		if e.Action != ActionCarry || e.Carried.Outcome != Accepted || counts.Call != 0 || counts.Carried != 1 {
			t.Fatalf("entry = %+v, counts %+v", e, counts)
		}
		if e.Carried.Sent != (Sent{Email: "ada@acme.io", Slug: "ada-lovelace"}) {
			t.Fatalf("the carried result stands for the current clues, sent %+v", e.Carried.Sent)
		}
		row := e.Carried.Row
		if row.ClueEmail != "ada@acme.io" || row.ClueLinkedInURL != clue.LinkedInURL || row.ClueName != "Ada Lovelace" {
			t.Fatalf("the row takes this load's clues and name: %+v", row)
		}
	})

	// A different profile behind the new URL means the email's match may be
	// someone else: look again.
	t.Run("a LinkedIn URL of another profile is looked up", func(t *testing.T) {
		clue := adaClue
		clue.LinkedInURL = "https://www.linkedin.com/in/ada-byron"
		entries, _ := plan(t, []Clue{clue}, accepted(Sent{Email: "ada@acme.io"}))
		if entries["c_17"].Action != ActionCall {
			t.Fatalf("entry = %+v", entries["c_17"])
		}
	})

	// A slug two people shared was never sent; once it is theirs alone it is.
	t.Run("a slug that has become unique is looked up", func(t *testing.T) {
		clue := Clue{Entity: "c_20", Name: "Alan Turing", LinkedInURL: "https://www.linkedin.com/in/alan-turing"}
		entries, _ := plan(t, []Clue{clue})
		if e := entries["c_20"]; e.Action != ActionCall || e.Sent.Slug != "alan-turing" {
			t.Fatalf("entry = %+v", e)
		}
	})

	// A new email on a carried profile is checked against its employers, for
	// free: an employer the profile doesn't list holds it, without a call.
	t.Run("a changed email is checked again for free", func(t *testing.T) {
		clue := Clue{Entity: "c_17", Name: "Ada Lovelace", Email: "ada@globex.com", LinkedInURL: "https://www.linkedin.com/in/ada-lovelace"}
		entries, counts := plan(t, []Clue{clue}, accepted(Sent{Email: "ada@acme.io", Slug: "ada-lovelace"}))
		e := entries["c_17"]
		if e.Action != ActionCarry || e.Carried.Outcome != Held || e.Carried.Reason != ReasonDomainMismatch || counts.Call != 0 {
			t.Fatalf("entry = %+v", e)
		}
		onlyEmail := Clue{Entity: "c_17", Name: "Ada Lovelace", Email: "ada@globex.com"}
		entries, _ = plan(t, []Clue{onlyEmail}, accepted(Sent{Email: "ada@acme.io"}))
		if entries["c_17"].Action != ActionCall {
			t.Fatalf("an email no one was looked up by is looked up: %+v", entries["c_17"])
		}
	})

	// The address now belongs to someone else: the gate holds the old match
	// without a call, and the name guard keeps it off the new person.
	t.Run("a renamed person is held name_mismatch without a call", func(t *testing.T) {
		clue := adaClue
		clue.Name = "Bob Smith"
		entries, counts := plan(t, []Clue{clue}, accepted(Sent{Email: "ada@acme.io"}))
		e := entries["c_17"]
		if e.Action != ActionCarry || e.Carried.Outcome != Held || e.Carried.Reason != ReasonNameMismatch || counts.Call != 0 {
			t.Fatalf("entry = %+v", e)
		}
	})

	// A match held because the name didn't agree passes once the data is
	// fixed, at no cost.
	t.Run("a held match is accepted for free once it passes", func(t *testing.T) {
		held := accepted(Sent{Email: "ada@acme.io"})
		held.Outcome, held.Reason = Held, ReasonNameMismatch
		entries, _ := plan(t, []Clue{adaClue}, held)
		if e := entries["c_17"]; e.Action != ActionCarry || e.Carried.Outcome != Accepted {
			t.Fatalf("entry = %+v", e)
		}
	})

	// Calls that may never have reached anyone, or that failed, cost
	// nothing to try again.
	for _, outcome := range []Outcome{Uncertain, Skipped} {
		t.Run(string(outcome)+" results are looked up again", func(t *testing.T) {
			entries, _ := plan(t, []Clue{adaClue}, Result{Sent: Sent{Email: "ada@acme.io"}, Outcome: outcome, CheckedAt: planNow})
			if entries["c_17"].Action != ActionCall {
				t.Fatalf("entry = %+v", entries["c_17"])
			}
		})
	}

	// An unfinished answer that held a match or found no one is asked again;
	// an accepted one is carried, still marked partial, so it is asked again
	// should the gate hold it later.
	t.Run("a partial answer is carried only when accepted", func(t *testing.T) {
		held := accepted(Sent{Email: "ada@acme.io"})
		held.Outcome, held.Reason, held.Partial = Held, ReasonDomainUnknown, true
		notFound := Result{Sent: Sent{Email: "ada@acme.io"}, Outcome: NotFound, CheckedAt: planNow, Partial: true}
		for _, r := range []Result{held, notFound} {
			if entries, counts := plan(t, []Clue{adaClue}, r); entries["c_17"].Action != ActionCall || counts.Call != 1 {
				t.Fatalf("%s partial = %+v", r.Outcome, entries["c_17"])
			}
		}
		kept := accepted(Sent{Email: "ada@acme.io"})
		kept.Partial = true
		entries, _ := plan(t, []Clue{adaClue}, kept)
		if e := entries["c_17"]; e.Action != ActionCarry || e.Carried.Outcome != Accepted || !e.Carried.Partial {
			t.Fatalf("accepted partial = %+v", e)
		}
		renamed := adaClue
		renamed.Name = "Bob Smith"
		entries, _ = plan(t, []Clue{renamed}, kept)
		if e := entries["c_17"]; e.Action != ActionCarry || e.Carried.Outcome != Held || !e.Carried.Partial {
			t.Fatalf("accepted partial, now held = %+v", e)
		}
	})

	// No one found is asked about again after 90 days, or as soon as there
	// is a clue that wasn't sent.
	t.Run("not_found is carried for 90 days and on the same clues", func(t *testing.T) {
		recent := Result{Sent: Sent{Email: "ada@acme.io"}, Outcome: NotFound, CheckedAt: planNow.AddDate(0, 0, -89)}
		entries, _ := plan(t, []Clue{adaClue}, recent)
		if e := entries["c_17"]; e.Action != ActionCarry || e.Carried.Outcome != NotFound || !e.Carried.CheckedAt.Equal(recent.CheckedAt) {
			t.Fatalf("a recent not_found = %+v", e)
		}
		old := recent
		old.CheckedAt = planNow.AddDate(0, 0, -91)
		entries, _ = plan(t, []Clue{adaClue}, old)
		if entries["c_17"].Action != ActionCall {
			t.Fatalf("an old not_found = %+v", entries["c_17"])
		}
		withURL := adaClue
		withURL.LinkedInURL = "https://www.linkedin.com/in/ada-lovelace"
		entries, _ = plan(t, []Clue{withURL}, recent)
		if entries["c_17"].Action != ActionCall {
			t.Fatalf("a new clue = %+v", entries["c_17"])
		}
	})

	// One result naming a person by both clues carries for them once.
	t.Run("a result matching both clues carries once", func(t *testing.T) {
		clue := adaClue
		clue.LinkedInURL = "https://www.linkedin.com/in/ada-lovelace"
		both := Sent{Email: "ada@acme.io", Slug: "ada-lovelace"}
		for _, r := range []Result{accepted(both), {Sent: both, Outcome: NotFound, CheckedAt: planNow}} {
			if entries, counts := plan(t, []Clue{clue}, r); entries["c_17"].Action != ActionCarry || counts.Carried != 1 {
				t.Fatalf("%s: entry = %+v counts %+v", r.Outcome, entries["c_17"], counts)
			}
		}
	})

	// Of two not_found results for a person, the later one read is carried;
	// one whose clues lack a clue the person sends now is not.
	t.Run("the last not_found read is carried", func(t *testing.T) {
		clue := adaClue
		clue.LinkedInURL = "https://www.linkedin.com/in/ada-lovelace"
		earlier := Result{Sent: Sent{Email: "ada@acme.io", Slug: "ada-lovelace"}, Outcome: NotFound, CheckedAt: planNow.AddDate(0, 0, -30)}
		later := Result{Sent: Sent{Slug: "ada-lovelace"}, Outcome: NotFound, CheckedAt: planNow.AddDate(0, 0, -3)}
		entries, _ := plan(t, []Clue{clue}, earlier, later)
		if e := entries["c_17"]; e.Action != ActionCarry || !e.Carried.CheckedAt.Equal(earlier.CheckedAt) {
			t.Fatalf("a later not_found without the email = %+v", e)
		}
		later.Sent.Email = "ada@acme.io"
		entries, counts := plan(t, []Clue{clue}, earlier, later)
		if e := entries["c_17"]; e.Action != ActionCarry || !e.Carried.CheckedAt.Equal(later.CheckedAt) || counts.Carried != 1 {
			t.Fatalf("entry = %+v counts %+v", e, counts)
		}
	})

	// Results are matched to the people they name, by email or by slug:
	// everyone else is called, in entity order after the carried.
	t.Run("carries for many people in one pass", func(t *testing.T) {
		var clues []Clue
		var previous []Result
		for i := range 40 {
			name := fmt.Sprintf("Person%c Number%c", 'a'+i%26, 'a'+i/26)
			c := Clue{Entity: fmt.Sprintf("c_%02d", i), Name: name, Email: fmt.Sprintf("p%d@acme.io", i)}
			if i%3 == 0 {
				c.LinkedInURL = fmt.Sprintf("https://www.linkedin.com/in/p-%d", i)
			}
			clues = append(clues, c)
			row := Profile{FullName: name, CurrentOrgDomain: "acme.io", LinkedInURL: c.LinkedInURL}
			found := Result{Outcome: Accepted, Score: score(96), ProfileID: fmt.Sprint(i), CheckedAt: planNow, Row: &row}
			switch {
			case i%4 == 0: // carried unless the person sends a slug it lacks
				previous = append(previous, Result{Sent: Sent{Email: c.Email}, Outcome: NotFound, CheckedAt: planNow})
			case i%4 == 1: // carried unless the person sends a slug its profile lacks
				row.LinkedInURL = ""
				found.Sent.Email = c.Email
				previous = append(previous, found)
			case i%4 == 2 && c.LinkedInURL != "":
				found.Sent.Slug = fmt.Sprintf("p-%d", i)
				previous = append(previous, found)
			}
		}
		entries, counts := planOf(t, nil, clues, previous...)
		collided, _ := planOf(t, collide, clues, previous...)
		if !reflect.DeepEqual(entries, collided) {
			t.Fatal("colliding hashes changed the plan")
		}
		carried := map[Outcome]int{}
		var called []string
		for _, e := range entries {
			switch e.Action {
			case ActionCarry:
				carried[e.Carried.Outcome]++
			case ActionCall:
				called = append(called, e.Entity)
			}
		}
		if carried[NotFound] != 6 || carried[Accepted] != 10 || counts.Call != 24 || counts.Carried != 16 || !slices.IsSorted(called) {
			t.Fatalf("carried %v, called %v, counts %+v", carried, called, counts)
		}
	})

	// A match read after a not_found for the same person wins: results are
	// read once, in file order.
	t.Run("a match wins over a not_found read before it", func(t *testing.T) {
		clue := adaClue
		clue.LinkedInURL = "https://www.linkedin.com/in/ada-lovelace"
		notFound := Result{Sent: Sent{Email: "ada@acme.io", Slug: "ada-lovelace"}, Outcome: NotFound, CheckedAt: planNow}
		entries, counts := plan(t, []Clue{clue}, notFound, accepted(Sent{Slug: "ada-lovelace"}))
		if e := entries["c_17"]; e.Action != ActionCarry || e.Carried.Outcome != Accepted || counts.Carried != 1 {
			t.Fatalf("entry = %+v counts %+v", e, counts)
		}
	})
}

// freshFile yields batch results as their files would.
func freshFile(results ...Fresh) func() iter.Seq2[Fresh, error] {
	return func() iter.Seq2[Fresh, error] {
		return func(yield func(Fresh, error) bool) {
			for _, r := range results {
				if !yield(r, nil) {
					return
				}
			}
		}
	}
}

func TestPublish(t *testing.T) {
	entries := []Entry{
		{Clue: Clue{Entity: "a"}, Action: ActionCarry, Carried: &Result{Outcome: Accepted, ProfileID: "li:ada", Row: &Profile{ProfileID: "li:ada"}}},
		{Clue: Clue{Entity: "b"}, Action: ActionCall},
		{Clue: Clue{Entity: "c"}, Action: ActionCall},
		{Clue: Clue{Entity: "d"}, Action: ActionCall},
		{Clue: Clue{Entity: "e"}, Action: ActionSkip, Reason: ReasonNoFullName},
		{Clue: Clue{Entity: "f"}, Action: ActionCarry, Carried: &Result{Outcome: NotFound}},
	}
	planFile := func() iter.Seq2[Entry, error] {
		return func(yield func(Entry, error) bool) {
			for _, e := range entries {
				if !yield(e, nil) {
					return
				}
			}
		}
	}
	// One profile on two people binds to neither; people never reached are
	// left out so the next load looks them up; skipped people are not results.
	t.Run("holds duplicates and leaves out the unreached", func(t *testing.T) {
		fresh := freshFile(
			Fresh{Entity: "b", Result: Result{Outcome: Accepted, ProfileID: "li:ada", Row: &Profile{ProfileID: "li:ada"}}},
			Fresh{Entity: "c", Result: Result{Outcome: Accepted, ProfileID: "li:grace", Row: &Profile{ProfileID: "li:grace"}}},
		)
		var results []Result
		pub, err := Publish(planFile, fresh, func(r Result) error { results = append(results, r); return nil })
		if err != nil {
			t.Fatal(err)
		}
		outcomes := slices.Collect(func(yield func(string) bool) {
			for _, r := range results {
				if !yield(string(r.Outcome) + ":" + r.Reason) {
					return
				}
			}
		})
		want := []string{"held:" + ReasonDuplicateProfile, "held:" + ReasonDuplicateProfile, "accepted:", "not_found:"}
		if !slices.Equal(outcomes, want) {
			t.Fatalf("results = %v, want %v", outcomes, want)
		}
		if pub.Profiles != 1 || pub.Fresh[Held] != 1 || pub.Fresh[Accepted] != 1 {
			t.Fatalf("published = %+v", pub)
		}
	})

	// A plan or a batch's results that can't be read again publish nothing.
	t.Run("stops at an unreadable plan or batch", func(t *testing.T) {
		broken := func() iter.Seq2[Entry, error] {
			return func(yield func(Entry, error) bool) { yield(Entry{}, errors.New("gone")) }
		}
		if _, err := Publish(broken, freshFile(), func(Result) error { return nil }); err == nil {
			t.Fatal("published from an unreadable plan")
		}
		brokenFresh := func() iter.Seq2[Fresh, error] {
			return func(yield func(Fresh, error) bool) { yield(Fresh{}, errors.New("gone")) }
		}
		if _, err := Publish(planFile, brokenFresh, func(Result) error { return nil }); err == nil {
			t.Fatal("published from an unreadable batch")
		}
	})

	// Results are joined to the plan in its order of calls; one out of that
	// order (or of no entity the plan calls) is a bug, never a silent drop.
	t.Run("refuses results out of the plan's order", func(t *testing.T) {
		fresh := freshFile(Fresh{Entity: "c", Result: Result{Outcome: NotFound}}, Fresh{Entity: "b", Result: Result{Outcome: NotFound}})
		if _, err := Publish(planFile, fresh, func(Result) error { return nil }); err == nil {
			t.Fatal("published results out of order")
		}
	})
}

// TestErrorsNameNoOne keeps people out of the errors a step can end with:
// the graph and enrichment workers log them, and logs hold no personal
// data. An entity's key may be its email, and a profile id holds a
// LinkedIn slug, so an error names a person, a result or a call by
// position.
func TestErrorsNameNoOne(t *testing.T) {
	ctx := context.Background()
	named := func(t *testing.T, err error, values ...string) {
		t.Helper()
		if err == nil {
			t.Fatal("no error")
		}
		for _, v := range values {
			if strings.Contains(err.Error(), v) {
				t.Fatalf("%q names %s", err, v)
			}
		}
	}

	// The people file sits on the worker's disk, which can fill: the person
	// whose line didn't fit is named by position.
	t.Run("a person the people file can't take", func(t *testing.T) {
		people, err := (&Files{Dir: t.TempDir()}).People(2)
		if err != nil {
			t.Fatal(err)
		}
		defer people.Close()
		if err := people.Add(Clue{Entity: "Person[grace@acme.io]", Name: "Grace Hopper"}); err != nil {
			t.Fatal(err)
		}
		_ = people.file.Close() // the disk fails under the buffered line
		err = people.Add(Clue{Entity: "Person[ada@acme.io]", Name: strings.Repeat("Ada ", 1<<14), Email: "ada@acme.io"})
		named(t, err, "acme.io")
		if !strings.Contains(err.Error(), "write person 1") {
			t.Fatalf("err = %v", err)
		}
	})

	// A result out of the plan's order of calls is a bug, logged when the
	// step fails.
	t.Run("a result out of the plan's order", func(t *testing.T) {
		plan := func() iter.Seq2[Entry, error] {
			return func(yield func(Entry, error) bool) {
				yield(Entry{Clue: Clue{Entity: "Person[grace@acme.io]"}, Action: ActionCall}, nil)
			}
		}
		fresh := freshFile(Fresh{Entity: "Person[ada@acme.io]", Result: Result{Outcome: NotFound}})
		_, err := Publish(plan, fresh, func(Result) error { return nil })
		named(t, err, "acme.io")
	})

	// An accepted result always carries its row; one without is a bug.
	t.Run("an accepted result with no row", func(t *testing.T) {
		files := &Files{Store: newMemStore(), Dir: t.TempDir()}
		if _, err := Put(ctx, files, "plan", []Entry{{Clue: Clue{Entity: "Person[ada@acme.io]"}, Action: ActionCall}}); err != nil {
			t.Fatal(err)
		}
		fresh := freshFile(Fresh{Entity: "Person[ada@acme.io]", Result: Result{Outcome: Accepted, ProfileID: "li:ada-lovelace"}})
		_, err := files.publish(ctx, &ontologyagent.Enrichment{Name: "people"}, "plan", fresh, "profiles", "")
		named(t, err, "acme.io", "ada-lovelace")
	})

	// A batch's calls answer its lookups in order; a call for another
	// entity is a bug.
	t.Run("a call for another entity than its lookup", func(t *testing.T) {
		_, err := (&run{cursor: 3}).judge([]Entry{{Clue: Clue{Entity: "Person[ada@acme.io]"}}},
			[]Call{{Entity: "Person[grace@acme.io]"}})
		named(t, err, "acme.io")
	})
}

// TestPlanMemory holds a plan's memory to PersonBytes a person, whatever
// their clues hold: no one's clues, and no earlier result's row, stay in
// memory. A third of the people carry a not_found, a third an accepted
// profile, the rest are called.
func TestPlanMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("plans 200,000 people")
	}
	const n = 200_000
	clue := func(i int) Clue {
		c := Clue{
			Entity: fmt.Sprintf("Person[contacts_%08d]", i), Name: fmt.Sprintf("Firstname%d Lastname%d", i, i),
			Email: fmt.Sprintf("firstname.lastname.%d@company-%d.example.com", i, i%5000),
		}
		if i%2 == 0 {
			c.LinkedInURL = fmt.Sprintf("https://www.linkedin.com/in/firstname-lastname-%d/", i)
		}
		return c
	}
	previous := func(yield func(Result, error) bool) {
		for i := range n {
			c := clue(i)
			var r Result
			switch i % 3 {
			case 0:
				r = Result{Sent: Sent{Email: c.Email}, Outcome: NotFound, CheckedAt: planNow}
			case 1:
				row := *ada()
				row.FullName, row.LinkedInURL = c.Name, c.LinkedInURL
				r = Result{Sent: Sent{Email: c.Email}, Outcome: Accepted, Score: score(96), ProfileID: fmt.Sprint(i), CheckedAt: planNow, Row: &row}
			default:
				continue
			}
			if !yield(r, nil) {
				return
			}
		}
	}
	heap := func() uint64 {
		var m runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&m)
		return m.HeapAlloc
	}
	before := heap()
	people, err := (&Files{Dir: t.TempDir()}).People(n)
	if err != nil {
		t.Fatal(err)
	}
	defer people.Close()
	for i := range n {
		if err := people.Add(clue(i)); err != nil {
			t.Fatal(err)
		}
	}
	peak := heap()
	emitted := 0
	counts, err := planner.Plan(people, previous, func(Entry) error {
		if emitted++; emitted == n-1 { // the last entries: every index and carried not_found still held
			peak = max(peak, heap())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if emitted != n || counts.Call+counts.Carried+counts.NoName+counts.NoClue != n {
		t.Fatalf("emitted %d, counts %+v", emitted, counts)
	}
	perPerson := float64(peak-min(peak, before)) / n
	t.Logf("%.1f bytes a person, counts %+v", perPerson, counts)
	if perPerson > PersonBytes*1.5 {
		t.Fatalf("a plan held %.1f bytes a person, more than %d and its slices' growth", perPerson, PersonBytes)
	}
}
