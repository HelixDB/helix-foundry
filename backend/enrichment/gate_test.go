package enrichment

import (
	"fmt"
	"testing"
)

func ExampleCanonicalLinkedIn() {
	slug, ok := CanonicalLinkedIn("https://uk.linkedin.com/in/Ada-Lovelace/details/experience/?trk=x#top")
	fmt.Println(slug, ok, LinkedInURL(slug))
	_, ok = CanonicalLinkedIn("https://www.linkedin.com/sales/lead/ACwAAA123,NAME")
	fmt.Println(ok)
	// Output:
	// ada-lovelace true https://www.linkedin.com/in/ada-lovelace
	// false
}

func TestCanonicalLinkedIn(t *testing.T) {
	// The canonical slug is what a profile is sent and carried by, so every
	// spelling of one profile must reach the same slug, and anything that
	// isn't one profile must be refused.
	for _, tc := range []struct {
		url  string
		slug string
	}{
		{"https://www.linkedin.com/in/ada-lovelace", "ada-lovelace"},
		{"linkedin.com/in/Ada-Lovelace/", "ada-lovelace"},
		{"  https://WWW.LinkedIn.com/in/ada-lovelace  ", "ada-lovelace"},
		{"http://uk.linkedin.com/in/ada-lovelace?trk=public_profile#experience", "ada-lovelace"},
		{"https://de.linkedin.com/in/ada-lovelace", "ada-lovelace"},
		{"https://m.linkedin.com/in/ada-lovelace/details/experience/", "ada-lovelace"},
		{"https://www.linkedin.com/in/j%C3%BCrgen-m%C3%BCller-1a2b3c", "jürgen-müller-1a2b3c"},
		{"https://www.linkedin.com/in/ada_lovelace-42/", "ada_lovelace-42"},
		{"https://www.linkedin.com/in/ACoAABcdEfGhIjK", ""},
		{"https://www.linkedin.com/sales/lead/ACwAAA123,NAME,abc", ""},
		{"https://www.linkedin.com/pub/ada-lovelace/1/2/3", ""},
		{"https://www.linkedin.com/company/acme", ""},
		{"https://www.linkedin.com/in/", ""},
		{"https://www.linkedin.com/in/ada-lovelace/recent-activity/", ""},
		{"https://linkedin.com.evil.example/in/ada-lovelace", ""},
		{"https://evil.example/in/ada-lovelace", ""},
		{"https://www.linkedin.com:8443/in/ada-lovelace", ""},
		{"ftp://www.linkedin.com/in/ada-lovelace", ""},
		{"https://user@www.linkedin.com/in/ada-lovelace", ""},
		{"https://www.linkedin.com/in/ada%20lovelace", ""},
		{"", ""},
		{"not a url at all", ""},
	} {
		t.Run(tc.url, func(t *testing.T) {
			slug, ok := CanonicalLinkedIn(tc.url)
			if ok != (tc.slug != "") || slug != tc.slug {
				t.Fatalf("CanonicalLinkedIn(%q) = %q, %t; want %q", tc.url, slug, ok, tc.slug)
			}
		})
	}
}

func TestMailboxLists(t *testing.T) {
	// A consumer address says nothing about the employer the gate checks, so
	// it is never sent, under any of its country domains.
	for _, domain := range []string{"gmail.com", "GMail.com", "yahoo.co.uk", "hotmail.fr", "outlook.com", "gmx.de", "proton.me", "icloud.com"} {
		t.Run("free mail "+domain, func(t *testing.T) {
			if !FreeMail(domain) {
				t.Fatalf("%s is not free mail", domain)
			}
		})
	}
	for _, domain := range []string{"acme.io", "mail.acme.co.uk", "yahoo-finance-partner.com", "", "co.uk"} {
		t.Run("work mail "+domain, func(t *testing.T) {
			if FreeMail(domain) {
				t.Fatalf("%s is free mail", domain)
			}
		})
	}
	// No one person reads a shared inbox.
	for _, local := range []string{"sales", "info", "support", "team", "billing", "admin", "hello", "contact", "noreply", "ap",
		"finance", "hr", "jobs", "office", "No-Reply", "no_reply", "Sales+eu", "h.r"} {
		t.Run("role "+local, func(t *testing.T) {
			if !RoleMailbox(local) {
				t.Fatalf("%s is not a role mailbox", local)
			}
		})
	}
	for _, local := range []string{"ada", "ada.lovelace", "salesforce", "apollo", "hrishi"} {
		t.Run("person "+local, func(t *testing.T) {
			if RoleMailbox(local) {
				t.Fatalf("%s is a role mailbox", local)
			}
		})
	}
}

func TestRegistrableDomain(t *testing.T) {
	// Step 3 compares registrable domains, so a mail subdomain or a country
	// suffix still matches the employer.
	for _, tc := range []struct{ host, want string }{
		{"acme.io", "acme.io"},
		{"Mail.Acme.io.", "acme.io"},
		{"www.acme.co.uk", "acme.co.uk"},
		{"eu.mail.acme.com.au", "acme.com.au"},
		{"co.uk", ""},
		{"", ""},
		{"ada@acme.io", ""},
	} {
		t.Run(tc.host, func(t *testing.T) {
			got, ok := RegistrableDomain(tc.host)
			if ok != (tc.want != "") || got != tc.want {
				t.Fatalf("RegistrableDomain(%q) = %q, %t; want %q", tc.host, got, ok, tc.want)
			}
		})
	}
}

// ada is the profile context.dev returns for Ada Lovelace of acme.io.
func ada() *Profile {
	return &Profile{
		FullName: "Ada Lovelace", FirstName: "Ada", LastName: "Lovelace", LinkedInURL: "https://www.linkedin.com/in/ada-lovelace",
		CurrentOrgDomain: "acme.io",
		Experience: []Experience{
			{ID: 1, Organization: Organization{Domain: "acme.io"}, IsCurrent: true},
			{ID: 2, Organization: Organization{Domain: "initech.com"}},
		},
	}
}

func TestGate(t *testing.T) {
	both := Sent{Email: "ada@mail.acme.io", Slug: "ada-lovelace"}
	// A match that agrees with everything sent and the person's name is applied.
	t.Run("accepts a match that agrees with the person", func(t *testing.T) {
		if outcome, reason := Gate(ada(), 96, 90, "Ada Lovelace", both); outcome != Accepted || reason != "" {
			t.Fatalf("gate = %s %s", outcome, reason)
		}
	})
	// The first failing step names the hold, so the step's order matters:
	// each case fails exactly one step.
	for _, tc := range []struct {
		name    string
		profile func(*Profile)
		score   int
		person  string
		sent    Sent
		reason  string
	}{
		{"a low score", nil, 89, "Ada Lovelace", both, ReasonLowScore},
		{"another last name", nil, 96, "Ada Byron", both, ReasonNameMismatch},
		{"another first name", nil, 96, "Augusta Lovelace", both, ReasonNameMismatch},
		{"a first name sharing two letters", func(p *Profile) { p.FirstName = "Adam" }, 96, "Adele Lovelace", both, ReasonNameMismatch},
		{"no name at all", func(p *Profile) { p.FirstName, p.LastName, p.FullName = "", "", "" }, 96, "Ada Lovelace", both, ReasonNameMismatch},
		{"an email at another employer", nil, 96, "Ada Lovelace", Sent{Email: "ada@globex.com"}, ReasonDomainMismatch},
		{"an email with no employer listed", func(p *Profile) { p.Experience, p.CurrentOrgDomain = nil, "" }, 96, "Ada Lovelace",
			Sent{Email: "ada@acme.io"}, ReasonDomainUnknown},
		{"another LinkedIn profile", nil, 96, "Ada Lovelace", Sent{Slug: "ada-byron"}, ReasonLinkedInMismatch},
		{"a profile without LinkedIn when a slug was sent", func(p *Profile) { p.LinkedInURL = "" }, 96, "Ada Lovelace",
			Sent{Slug: "ada-lovelace"}, ReasonLinkedInMismatch},
	} {
		t.Run("holds "+tc.name, func(t *testing.T) {
			p := ada()
			if tc.profile != nil {
				tc.profile(p)
			}
			if outcome, reason := Gate(p, tc.score, 90, tc.person, tc.sent); outcome != Held || reason != tc.reason {
				t.Fatalf("gate = %s %s, want held %s", outcome, reason, tc.reason)
			}
		})
	}
	// Names match the way people write them: accents, a middle name, a
	// shortened first name, a hyphenated or full name only.
	for _, tc := range []struct {
		name    string
		profile func(*Profile)
		person  string
	}{
		{"accents stripped", func(p *Profile) { p.FirstName, p.LastName = "José", "García" }, "Jose Garcia"},
		{"a middle name", nil, "Augusta Ada King Lovelace"},
		{"a shortened first name", func(p *Profile) { p.FirstName = "Alexandra" }, "Alex Lovelace"},
		{"a hyphenated last name", func(p *Profile) { p.LastName = "King-Lovelace" }, "Ada King-Lovelace"},
		{"the full name only", func(p *Profile) { p.FirstName, p.LastName = "", "" }, "Ada Lovelace"},
	} {
		t.Run("accepts "+tc.name, func(t *testing.T) {
			p := ada()
			if tc.profile != nil {
				tc.profile(p)
			}
			if outcome, reason := Gate(p, 96, 90, tc.person, Sent{Slug: "ada-lovelace"}); outcome != Accepted {
				t.Fatalf("gate = %s %s", outcome, reason)
			}
		})
	}
}
