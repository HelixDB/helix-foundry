package enrichment

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/publicsuffix"
	"golang.org/x/text/unicode/norm"
)

// linkedInHost is linkedin.com with an optional locale (uk.), mobile (m.) or
// www. prefix.
var linkedInHost = regexp.MustCompile(`^(?:[a-z]{2}\.|m\.|www\.)?linkedin\.com$`)

// CanonicalLinkedIn returns the profile slug of a LinkedIn profile URL: the
// URL must be linkedin.com/in/<slug>, with an optional scheme, locale or m.
// subdomain, query, fragment, trailing slash or /details/… path, all of which
// are dropped. The slug is percent-decoded and lowercased. Sales Navigator
// (/sales/), public directory (/pub/) and member-id (ACoAA…) URLs are
// refused: they don't name one profile stably.
func CanonicalLinkedIn(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Port() != "" {
		return "", false
	}
	if !linkedInHost.MatchString(strings.ToLower(u.Hostname())) {
		return "", false
	}
	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segments) < 2 || segments[0] != "in" || (len(segments) > 2 && segments[2] != "details") {
		return "", false
	}
	slug := segments[1]
	if strings.HasPrefix(slug, "ACoAA") || utf8.RuneCountInString(slug) > 100 || !utf8.ValidString(slug) ||
		strings.ContainsFunc(slug, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) || slug == "" {
		return "", false
	}
	return strings.ToLower(slug), true
}

// LinkedInURL is the canonical URL of a profile slug.
func LinkedInURL(slug string) string {
	return "https://www.linkedin.com/in/" + url.PathEscape(slug)
}

// freeMailDomains are consumer mail providers: a person's address there says
// nothing about their employer, so it is not sent.
var freeMailDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "ymail.com": true, "rocketmail.com": true, "icloud.com": true, "me.com": true,
	"mac.com": true, "proton.me": true, "protonmail.com": true, "protonmail.ch": true, "pm.me": true, "web.de": true,
	"mail.com": true, "email.com": true, "mail.ru": true, "bk.ru": true, "inbox.ru": true, "list.ru": true, "qq.com": true,
	"163.com": true, "126.com": true, "sina.com": true, "zoho.com": true, "fastmail.com": true, "hey.com": true,
	"tutanota.com": true, "tuta.io": true, "comcast.net": true, "verizon.net": true, "att.net": true, "sbcglobal.net": true,
	"btinternet.com": true, "orange.fr": true, "free.fr": true, "laposte.net": true, "libero.it": true, "seznam.cz": true,
	"naver.com": true, "daum.net": true, "rediffmail.com": true, "t-online.de": true, "freenet.de": true, "wp.pl": true,
	"o2.pl": true, "interia.pl": true, "abv.bg": true, "rambler.ru": true,
}

// freeMailBrands are consumer providers under many country domains
// (yahoo.co.uk, hotmail.fr, gmx.de), matched on the registrable domain's
// first label.
var freeMailBrands = map[string]bool{
	"yahoo": true, "hotmail": true, "outlook": true, "live": true, "msn": true, "aol": true, "gmx": true, "yandex": true,
}

// FreeMail reports a consumer mail domain.
func FreeMail(domain string) bool {
	registrable, ok := RegistrableDomain(domain)
	if !ok {
		return false
	}
	brand, _, _ := strings.Cut(registrable, ".")
	return freeMailDomains[registrable] || freeMailBrands[brand]
}

// roleMailboxes are shared inboxes: no one person reads them.
var roleMailboxes = map[string]bool{
	"sales": true, "info": true, "support": true, "team": true, "billing": true, "admin": true, "hello": true,
	"contact": true, "noreply": true, "ap": true, "finance": true, "hr": true, "jobs": true, "office": true,
}

// RoleMailbox reports a shared mailbox's local part, ignoring case, a +tag
// and the separators . - _ (no-reply is noreply).
func RoleMailbox(local string) bool {
	local, _, _ = strings.Cut(strings.ToLower(local), "+")
	return roleMailboxes[strings.NewReplacer(".", "", "-", "", "_", "").Replace(local)]
}

// RegistrableDomain is the part of a host a registrant owns, its public
// suffix plus one label (mail.acme.co.uk is acme.co.uk), lowercased and
// without www. False for a host that is itself a public suffix, or none.
func RegistrableDomain(host string) (string, bool) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" || strings.ContainsAny(host, "/@ :") {
		return "", false
	}
	domain, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil {
		return "", false
	}
	return domain, true
}

// NameTokens are a name's letter tokens: accents stripped, lowercased, split
// at anything that is not a letter.
func NameTokens(name string) []string {
	var b strings.Builder
	for _, r := range norm.NFD.String(name) {
		if !unicode.Is(unicode.Mn, r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return strings.FieldsFunc(b.String(), func(r rune) bool { return !unicode.IsLetter(r) })
}

// Outcome is how one person's lookup ended.
type Outcome string

const (
	// Accepted is a match that passed the gate: its profile is applied.
	Accepted Outcome = "accepted"
	// Held is a match that failed the gate (Reason says which step): it is
	// charged and kept, and never applied.
	Held Outcome = "held"
	// NotFound is the provider finding no one.
	NotFound Outcome = "not_found"
	// Uncertain is a call that got no answer and may have been charged.
	Uncertain Outcome = "uncertain"
	// Skipped is a call the provider refused or kept failing: free.
	Skipped Outcome = "skipped"
)

// Reasons a match is held, in the gate's order, and a call skipped.
const (
	ReasonLowScore         = "low_score"
	ReasonNameMismatch     = "name_mismatch"
	ReasonDomainMismatch   = "domain_mismatch"
	ReasonDomainUnknown    = "domain_unknown"
	ReasonLinkedInMismatch = "linkedin_mismatch"
	ReasonDuplicateProfile = "duplicate_profile"
	ReasonRefused          = "refused"
	ReasonProviderError    = "provider_error"
)

// Gate checks a match against the person it is for: its entity's name and
// the clues sent. The first step that fails holds it:
//  1. score at least minScore (low_score);
//  2. the match's last name among the name's tokens, and its first name
//     equal to one or sharing a prefix of three letters or more
//     (name_mismatch);
//  3. with an email sent, its registrable domain among the experience's
//     (domain_mismatch, or domain_unknown when the profile names none);
//  4. with a slug sent, the slug among the profile's (linkedin_mismatch).
//
// A profile on two or more people is held at publish (duplicate_profile).
func Gate(p *Profile, score, minScore int, name string, sent Sent) (Outcome, string) {
	if score < minScore {
		return Held, ReasonLowScore
	}
	tokens := NameTokens(name)
	last, first := NameTokens(p.LastName), NameTokens(p.FirstName)
	if len(last) == 0 || len(first) == 0 {
		full := NameTokens(p.FullName)
		if len(full) < 2 {
			return Held, ReasonNameMismatch
		}
		first, last = full[:1], full[len(full)-1:]
	}
	lastMatches := !slices.ContainsFunc(last, func(t string) bool { return !slices.Contains(tokens, t) })
	firstMatches := slices.ContainsFunc(tokens, func(t string) bool { return t == first[0] || commonPrefix(t, first[0]) >= 3 })
	if !lastMatches || !firstMatches {
		return Held, ReasonNameMismatch
	}
	if sent.Email != "" {
		_, domain, _ := strings.Cut(sent.Email, "@")
		registrable, _ := RegistrableDomain(domain)
		domains := p.domains()
		switch {
		case len(domains) == 0:
			return Held, ReasonDomainUnknown
		case registrable == "" || !slices.Contains(domains, registrable):
			return Held, ReasonDomainMismatch
		}
	}
	if sent.Slug != "" {
		if slug, ok := CanonicalLinkedIn(p.LinkedInURL); !ok || slug != sent.Slug {
			return Held, ReasonLinkedInMismatch
		}
	}
	return Accepted, ""
}

// commonPrefix is how many leading runes a and b share.
func commonPrefix(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	n := 0
	for n < len(ra) && n < len(rb) && ra[n] == rb[n] {
		n++
	}
	return n
}
