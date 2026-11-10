// Package reminder is the schedule of the reminders a person asks for on
// Foundry's landing page, from a phone, to try Foundry on a computer: when
// each one goes, in the person's own time zone, and the address it goes to.
// It is pure. The store keeps each address's schedule (foundry_reminders),
// the API takes new ones from the foundry-events door, and the
// foundry-notify-worker sends them on ports.TopicFoundryReminder. The site's
// mock follows the same goldens (testdata/schedule.json).
//
// Every reminder goes at 09:00 on a weekday, in the time zone the person's
// browser named. The first goes at the first such time at least Lead from
// when it was asked for; each later one at the first such time on or after
// the day a number of days after the one before it (After). Counting days,
// not hours, keeps a reminder at 09:00 across a change of daylight saving.
package reminder

import (
	"errors"
	"net/mail"
	"strings"
	"time"
	"unicode"
	// Time zones come with the binary, so a host image without tzdata
	// schedules by the person's zone all the same.
	_ "time/tzdata"
)

const (
	// Hour is the local hour every reminder goes at.
	Hour = 9
	// Lead is the least time before the first reminder: one asked for at
	// 08:30 goes on the next weekday, not in half an hour.
	Lead = time.Hour
	// Consent is the only consent a request may carry: the site's words
	// "By sending, you're agreeing for us to use your email to remind you
	// about Foundry", as first shown. A new wording is a new version.
	Consent = "remind-v1"
	// MaxEmail is the longest address taken, in bytes: the most a path of
	// SMTP carries.
	MaxEmail = 254
	// MaxZone is the longest time zone name taken, in bytes.
	MaxZone = 64
)

// ErrEmail is an address Normalize refuses. Its text never quotes the
// address.
var ErrEmail = errors.New("email must be one address, such as name@company.com, of at most 254 characters and with no name or comment")

// Normalize is the address a reminder goes to, as it is kept: email
// trimmed and lowercased, once it is one plain address (local@domain, the
// domain with a dot), with no display name, comment, quoted local part or
// control character, of at most MaxEmail bytes. Lowercasing makes one row
// of an address however it was typed; no provider Foundry's people use
// tells addresses apart by case.
func Normalize(email string) (string, error) {
	email = strings.TrimSpace(email)
	if email == "" || len(email) > MaxEmail || strings.ContainsAny(email, "\"(),:;<>[\\] ") ||
		strings.IndexFunc(email, unicode.IsControl) >= 0 || strings.Count(email, "@") != 1 {
		return "", ErrEmail
	}
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Name != "" || parsed.Address != email {
		return "", ErrEmail
	}
	local, domain, _ := strings.Cut(email, "@")
	if local == "" || !strings.Contains(domain, ".") || strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") ||
		strings.Contains(domain, "..") {
		return "", ErrEmail
	}
	return strings.ToLower(email), nil
}

// Zone is the time zone a reminder is scheduled in, and its name as kept:
// the IANA zone name names, or UTC for one Foundry doesn't know, an empty
// one, Local (the server's, which is no one's) or a name past MaxZone. The
// site sends the zone the browser resolves, so UTC is a rare fallback, and
// never a refusal: a reminder at 09:00 UTC still reaches the person.
func Zone(name string) (*time.Location, string) {
	name = strings.TrimSpace(name)
	if name == "" || name == "Local" || len(name) > MaxZone {
		return time.UTC, "UTC"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC, "UTC"
	}
	return loc, loc.String()
}

// First is when the first reminder asked for at now goes: the first
// weekday at Hour in loc at or after now+Lead.
func First(now time.Time, loc *time.Location) time.Time {
	return atOrAfter(now.Add(Lead), loc)
}

// After is when the reminder days after last goes: the first weekday at
// Hour in loc on or after the day days after last's day, in loc. A gap
// counted in days keeps the reminder at Hour whatever daylight saving does
// in between.
func After(last time.Time, days int, loc *time.Location) time.Time {
	y, m, d := last.In(loc).Date()
	return atOrAfter(time.Date(y, m, d+days, 0, 0, 0, 0, loc), loc)
}

// atOrAfter is the first weekday at Hour in loc at or after from, in UTC.
// Hour exists on every day of every zone (no zone changes its clocks then),
// so each day has its time.
func atOrAfter(from time.Time, loc *time.Location) time.Time {
	y, m, d := from.In(loc).Date()
	for day := 0; ; day++ {
		at := time.Date(y, m, d+day, Hour, 0, 0, 0, loc)
		if at.Weekday() == time.Saturday || at.Weekday() == time.Sunday || at.Before(from) {
			continue
		}
		return at.UTC()
	}
}
