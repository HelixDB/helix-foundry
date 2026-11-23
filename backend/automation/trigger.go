package automation

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	// Time zones come with the binary, so a host image without tzdata
	// schedules in the owner's zone all the same.
	_ "time/tzdata"

	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// Trigger is what starts an automation's runs: a closed set. A schedule
// fires at its times; every other trigger but Manual is an event Foundry
// finds where its work finished; Manual runs only when its owner asks (any
// automation can be run by hand too).
type Trigger interface{ triggerKind() TriggerKind }

// TriggerKind names a trigger's kind as the spec writes it.
type TriggerKind string

const (
	TriggerSchedule           TriggerKind = "schedule"
	TriggerSyncFinished       TriggerKind = "sync_finished"
	TriggerLoadReady          TriggerKind = "load_ready"
	TriggerDocumentArrived    TriggerKind = "document_arrived"
	TriggerGenerationFinished TriggerKind = "generation_finished"
	TriggerReviewsWaiting     TriggerKind = "reviews_waiting"
	TriggerManual             TriggerKind = "manual"
)

// Schedule fires every hour at Minute, every day at a time of day, or every
// week on a day at a time of day, in Zone: never a raw cron.
type Schedule struct {
	Every Every
	// Minute is the minute past the hour, for Every hour.
	Minute int
	// Hour and Minute are the time of day, for Every day and week.
	Hour int
	// Weekday is the day, for Every week.
	Weekday time.Weekday
	// Zone is the IANA time zone its times are in, and ZoneName its name.
	Zone     *time.Location
	ZoneName string
}

// Every is how often a schedule fires: a closed set.
type Every string

const (
	EveryHour Every = "hour"
	EveryDay  Every = "day"
	EveryWeek Every = "week"
)

// Outcome is which ends of a sync or a generation fire a trigger: a closed set.
type Outcome string

const (
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeFailed    Outcome = "failed"
	OutcomeAny       Outcome = "any"
)

// SyncFinished fires when a sync of the connector ends as Outcome says.
type SyncFinished struct {
	ConnectorID string
	Outcome     Outcome
}

// LoadReady fires when a load of the ontology finishes ready (or partial:
// written, with some documents unread).
type LoadReady struct{ OntologyID string }

// DocumentArrived fires when documents arrive in the set: uploaded, or
// written by a connector's sync.
type DocumentArrived struct{ SetID string }

// GenerationFinished fires when a draft of the ontology (not a revision)
// ends as Outcome says.
type GenerationFinished struct {
	OntologyID string
	Outcome    Outcome
}

// ReviewsWaiting fires on state, not on ended work: when the ontology's
// current load has at least AtLeast review cards open, once its review index
// is whole and its count fresh. It fires once per arming: for the load that
// is current when it is turned on or resumed (when enough wait already), for
// each new current load, and for the same load again only after its count fell
// below AtLeast and rose past it. Its event and output carry ids and numbers
// only, never a document's or a review's text, so a Founder step after it
// reads nothing untrusted from it.
type ReviewsWaiting struct {
	OntologyID string
	AtLeast    int
}

// MaxReviewsWaiting is the most a ReviewsWaiting trigger waits for: the
// review index counts open cards up to it.
const MaxReviewsWaiting = 10000

// CardsToReview writes a count of open review cards as a person reads it,
// grouped by thousands, and "10,000 or more" at MaxReviewsWaiting, past
// which the index stops counting.
//
//	automation.CardsToReview(1214) // "1,214"
func CardsToReview(n int64) string {
	if n >= MaxReviewsWaiting {
		return message.NewPrinter(language.English).Sprintf("%d or more", MaxReviewsWaiting)
	}
	return message.NewPrinter(language.English).Sprintf("%d", n)
}

// Manual never fires: its owner runs it.
type Manual struct{}

func (Schedule) triggerKind() TriggerKind           { return TriggerSchedule }
func (SyncFinished) triggerKind() TriggerKind       { return TriggerSyncFinished }
func (LoadReady) triggerKind() TriggerKind          { return TriggerLoadReady }
func (DocumentArrived) triggerKind() TriggerKind    { return TriggerDocumentArrived }
func (GenerationFinished) triggerKind() TriggerKind { return TriggerGenerationFinished }
func (ReviewsWaiting) triggerKind() TriggerKind     { return TriggerReviewsWaiting }
func (Manual) triggerKind() TriggerKind             { return TriggerManual }

// KindOf is t's kind.
func KindOf(t Trigger) TriggerKind { return t.triggerKind() }

// Target is the id an event trigger listens to; "" for a schedule or Manual.
func Target(t Trigger) string {
	switch t := t.(type) {
	case SyncFinished:
		return t.ConnectorID
	case LoadReady:
		return t.OntologyID
	case DocumentArrived:
		return t.SetID
	case GenerationFinished:
		return t.OntologyID
	case ReviewsWaiting:
		return t.OntologyID
	}
	return ""
}

// clock is a time of day as a schedule writes it.
var clock = regexp.MustCompile(`^([01][0-9]|2[0-3]):([0-5][0-9])$`)

var weekdays = map[string]time.Weekday{
	"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday,
	"thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday,
}

type wireTrigger struct {
	Kind        TriggerKind `json:"kind"`
	Every       Every       `json:"every,omitempty"`
	Minute      *int        `json:"minute,omitempty"`
	Weekday     string      `json:"weekday,omitempty"`
	At          string      `json:"at,omitempty"`
	TimeZone    string      `json:"time_zone,omitempty"`
	ConnectorID string      `json:"connector_id,omitempty"`
	OntologyID  string      `json:"ontology_id,omitempty"`
	SetID       string      `json:"set_id,omitempty"`
	Outcome     Outcome     `json:"outcome,omitempty"`
	AtLeast     *int        `json:"at_least,omitempty"`
}

// fields lists what a trigger of each kind holds, for its errors.
var triggerFields = map[TriggerKind]string{
	TriggerSchedule:           `{kind: "schedule", every: "hour", minute: 0 to 59, time_zone}, {kind: "schedule", every: "day", at: "HH:MM", time_zone} or {kind: "schedule", every: "week", weekday: "monday" … "sunday", at: "HH:MM", time_zone}`,
	TriggerSyncFinished:       `{kind: "sync_finished", connector_id, outcome: "succeeded" | "failed" | "any"}`,
	TriggerLoadReady:          `{kind: "load_ready", ontology_id}`,
	TriggerDocumentArrived:    `{kind: "document_arrived", set_id}`,
	TriggerGenerationFinished: `{kind: "generation_finished", ontology_id, outcome: "succeeded" | "failed" | "any"}`,
	TriggerReviewsWaiting:     `{kind: "reviews_waiting", ontology_id, at_least: 1 to 10000}`,
	TriggerManual:             `{kind: "manual"}`,
}

func parseTrigger(raw json.RawMessage) (Trigger, error) {
	kind, err := kindOf(raw, "kind")
	if err != nil {
		return nil, fmt.Errorf("a trigger: %w", err)
	}
	shape, known := triggerFields[TriggerKind(kind)]
	if !known {
		return nil, fmt.Errorf("a trigger's kind is %q; it is schedule, sync_finished, load_ready, document_arrived, generation_finished, reviews_waiting or manual", kind)
	}
	var w wireTrigger
	if err := decodeStrict(raw, &w); err != nil {
		return nil, fmt.Errorf("a %s trigger is %s: %w", kind, shape, err)
	}
	wrong := fmt.Errorf("a %s trigger is %s", kind, shape)
	// Exactly its own fields: a field of another kind is a mistake, never ignored.
	got := w
	got.Kind = ""
	switch w.Kind {
	case TriggerSchedule:
		return parseSchedule(w, wrong)
	case TriggerSyncFinished:
		if got != (wireTrigger{ConnectorID: w.ConnectorID, Outcome: w.Outcome}) || !idPattern.MatchString(w.ConnectorID) || !validOutcome(w.Outcome) {
			return nil, wrong
		}
		return SyncFinished{ConnectorID: w.ConnectorID, Outcome: w.Outcome}, nil
	case TriggerLoadReady:
		if got != (wireTrigger{OntologyID: w.OntologyID}) || !idPattern.MatchString(w.OntologyID) {
			return nil, wrong
		}
		return LoadReady{OntologyID: w.OntologyID}, nil
	case TriggerDocumentArrived:
		if got != (wireTrigger{SetID: w.SetID}) || !idPattern.MatchString(w.SetID) {
			return nil, wrong
		}
		return DocumentArrived{SetID: w.SetID}, nil
	case TriggerGenerationFinished:
		if got != (wireTrigger{OntologyID: w.OntologyID, Outcome: w.Outcome}) || !idPattern.MatchString(w.OntologyID) || !validOutcome(w.Outcome) {
			return nil, wrong
		}
		return GenerationFinished{OntologyID: w.OntologyID, Outcome: w.Outcome}, nil
	case TriggerReviewsWaiting:
		if got != (wireTrigger{OntologyID: w.OntologyID, AtLeast: w.AtLeast}) || !idPattern.MatchString(w.OntologyID) || w.AtLeast == nil || *w.AtLeast < 1 || *w.AtLeast > MaxReviewsWaiting {
			return nil, wrong
		}
		return ReviewsWaiting{OntologyID: w.OntologyID, AtLeast: *w.AtLeast}, nil
	default:
		if got != (wireTrigger{}) {
			return nil, wrong
		}
		return Manual{}, nil
	}
}

func validOutcome(o Outcome) bool {
	return o == OutcomeSucceeded || o == OutcomeFailed || o == OutcomeAny
}

func parseSchedule(w wireTrigger, wrong error) (Schedule, error) {
	if w.ConnectorID != "" || w.OntologyID != "" || w.SetID != "" || w.Outcome != "" || w.AtLeast != nil {
		return Schedule{}, wrong
	}
	zone, err := Zone(w.TimeZone)
	if err != nil {
		return Schedule{}, err
	}
	s := Schedule{Every: w.Every, Zone: zone, ZoneName: w.TimeZone}
	switch w.Every {
	case EveryHour:
		if w.Minute == nil || *w.Minute < 0 || *w.Minute > 59 || w.At != "" || w.Weekday != "" {
			return Schedule{}, wrong
		}
		s.Minute = *w.Minute
		return s, nil
	case EveryDay, EveryWeek:
		m := clock.FindStringSubmatch(w.At)
		if m == nil || w.Minute != nil || (w.Every == EveryDay) != (w.Weekday == "") {
			return Schedule{}, wrong
		}
		// The pattern holds two digits each.
		s.Hour, _ = strconv.Atoi(m[1])
		s.Minute, _ = strconv.Atoi(m[2])
		if w.Every == EveryWeek {
			day, ok := weekdays[w.Weekday]
			if !ok {
				return Schedule{}, wrong
			}
			s.Weekday = day
		}
		return s, nil
	}
	return Schedule{}, wrong
}

// Zone reads an IANA time zone name, refusing "", Local (the server's, which
// is no one's) and any name Go's zone database doesn't have.
func Zone(name string) (*time.Location, error) {
	if name == "" || name == "Local" || len(name) > 64 || strings.ContainsAny(name, " \t\n") {
		return nil, fmt.Errorf("time_zone %q is not an IANA time zone, such as Europe/London", name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("time_zone %q is not an IANA time zone, such as Europe/London", name)
	}
	return loc, nil
}

// Next is the schedule's first time strictly after after. A day's or a
// week's time keeps to the wall clock across a change of daylight saving; a
// time a change skips fires at the time Go's calendar normalizes it to (an
// hour later, in most zones), and a time a change repeats fires once.
//
//	next := schedule.Next(time.Now())
func (s Schedule) Next(after time.Time) time.Time {
	local := after.In(s.Zone)
	y, mo, d := local.Date()
	switch s.Every {
	case EveryHour:
		at := time.Date(y, mo, d, local.Hour(), s.Minute, 0, 0, s.Zone)
		for !at.After(after) {
			at = at.Add(time.Hour)
		}
		return at
	case EveryDay:
		at := time.Date(y, mo, d, s.Hour, s.Minute, 0, 0, s.Zone)
		for day := 1; !at.After(after); day++ {
			at = time.Date(y, mo, d+day, s.Hour, s.Minute, 0, 0, s.Zone)
		}
		return at
	default:
		ahead := (int(s.Weekday) - int(local.Weekday()) + 7) % 7
		at := time.Date(y, mo, d+ahead, s.Hour, s.Minute, 0, 0, s.Zone)
		if !at.After(after) {
			at = time.Date(y, mo, d+ahead+7, s.Hour, s.Minute, 0, 0, s.Zone)
		}
		return at
	}
}

// wireTriggerOf is a trigger as the spec writes it.
func wireTriggerOf(t Trigger) wireTrigger {
	switch t := t.(type) {
	case Schedule:
		w := wireTrigger{Kind: TriggerSchedule, Every: t.Every, TimeZone: t.ZoneName}
		switch t.Every {
		case EveryHour:
			minute := t.Minute
			w.Minute = &minute
		case EveryWeek:
			w.Weekday = strings.ToLower(t.Weekday.String())
			fallthrough
		default:
			w.At = fmt.Sprintf("%02d:%02d", t.Hour, t.Minute)
		}
		return w
	case SyncFinished:
		return wireTrigger{Kind: TriggerSyncFinished, ConnectorID: t.ConnectorID, Outcome: t.Outcome}
	case LoadReady:
		return wireTrigger{Kind: TriggerLoadReady, OntologyID: t.OntologyID}
	case DocumentArrived:
		return wireTrigger{Kind: TriggerDocumentArrived, SetID: t.SetID}
	case GenerationFinished:
		return wireTrigger{Kind: TriggerGenerationFinished, OntologyID: t.OntologyID, Outcome: t.Outcome}
	case ReviewsWaiting:
		atLeast := t.AtLeast
		return wireTrigger{Kind: TriggerReviewsWaiting, OntologyID: t.OntologyID, AtLeast: &atLeast}
	}
	return wireTrigger{Kind: TriggerManual}
}
