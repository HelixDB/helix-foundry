// Package companyresearch is how a new ontology's wizard starts from the
// person's company: their domain (NormalizeDomain), its website read for
// what it says of the company (Fetcher), a model's research of it (the
// research prompt, then ParseResearch of its one tool call: who the company
// is and up to MaxQuestions questions a website can't answer, each with a
// best guess), and, once the person has answered, a goal drafted from the
// answers and the data they chose (the goal prompt, then ParseGoal).
//
// A website and a person wrote what the model reads, and a model wrote what
// Parse keeps: every prompt puts them in blocks it calls data, and every
// string kept is cut to its cap. Nothing here talks to a store or a model;
// only the Fetcher reaches the network, through connector.Guard.
package companyresearch

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strings"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"

	"github.com/helixdb/helix-foundry/backend/enrichment"
)

// The caps of what a research and a goal hold, in characters (runes).
const (
	MaxCompanyName   = 120
	MaxSummary       = 600
	MaxName          = 60
	MaxQuestions     = 5
	MaxQuestion      = 200
	MaxBestGuess     = 300
	MaxOptions       = 4
	MaxOption        = 80
	MaxAnswer        = 500
	MaxData          = 12000
	MaxGoal          = 2000
	maxDomainInput   = 2048
	maxDomainASCII   = 253
	maxURL           = 2048
	questionIDPrefix = "q"
)

// Research is a model's research of a company's website, as it is stored
// and answered: every field within its cap, questions q1 to qN in order.
type Research struct {
	CompanyName   string     `json:"company_name"`
	Summary       string     `json:"summary"`
	SuggestedName string     `json:"suggested_name"`
	Questions     []Question `json:"questions"`
	// Sources are the pages read, home page first.
	Sources []string `json:"sources"`
	// Fetched is whether any page could be read.
	Fetched bool `json:"fetched"`
}

// Question is something a website can't say, with the model's best guess
// and up to MaxOptions answers to choose from.
type Question struct {
	ID        string   `json:"id"`
	Question  string   `json:"question"`
	BestGuess string   `json:"best_guess"`
	Options   []string `json:"options"`
}

// Goal is a drafted goal and a name for the ontology.
type Goal struct {
	Goal string `json:"goal"`
	Name string `json:"name"`
}

// DomainError is a domain refused, in a sentence the person can be shown.
type DomainError string

func (e DomainError) Error() string { return string(e) }

const (
	errNoDomain   DomainError = "Enter your company’s website, such as acme.com."
	errNotDomain  DomainError = "That isn’t a website’s domain: enter your company’s, such as acme.com."
	errIPAddress  DomainError = "Enter your company’s domain, such as acme.com, not an IP address."
	errPersonal   DomainError = "That’s a personal email provider’s domain: enter your company’s website instead."
	errNoRegistry DomainError = "That domain isn’t on the public internet: enter your company’s website, such as acme.com."
)

// NormalizeDomain is the registrable domain of a website or domain as a
// person types it: trimmed and lowercased, its scheme, user, path, query,
// fragment, port and trailing dot dropped, an email address's domain
// taken, international names in ASCII (IDNA), then reduced to the public
// suffix and one label ("www.shop.acme.co.uk" is "acme.co.uk"). An IP
// address, a name under no public suffix, and a personal email provider's
// domain (enrichment.FreeMail) are a DomainError.
func NormalizeDomain(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch {
	case s == "":
		return "", errNoDomain
	case len(s) > maxDomainInput:
		return "", errNotDomain
	}
	host := s
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return "", errNotDomain
		}
		host = u.Host
	} else {
		host, _, _ = strings.Cut(host, "/")
		host, _, _ = strings.Cut(host, "?")
		host, _, _ = strings.Cut(host, "#")
	}
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if _, err := netip.ParseAddr(host); err == nil {
		return "", errIPAddress
	}
	ascii, err := idna.Lookup.ToASCII(host)
	if err != nil || ascii == "" || len(ascii) > maxDomainASCII || !strings.Contains(ascii, ".") {
		return "", errNotDomain
	}
	domain, err := publicsuffix.EffectiveTLDPlusOne(ascii)
	if err != nil {
		return "", errNotDomain
	}
	// A suffix no list names is read as the name's last label, which no
	// public website is under (acme.internal, acme.local).
	if suffix, icann := publicsuffix.PublicSuffix(domain); !icann && !strings.Contains(suffix, ".") {
		return "", errNoRegistry
	}
	if enrichment.FreeMail(domain) {
		return "", errPersonal
	}
	return domain, nil
}

// errInvalid is a model's answer Parse keeps nothing of.
var errInvalid = errors.New("companyresearch: the model's answer isn't usable")
