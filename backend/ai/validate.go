package ai

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/helixdb/helix-foundry/backend/connector"
)

// The rules a saved provider meets, as the agent's contract checks them
// (models::check): each error names the field, never its value, so a key
// pasted into the wrong field is not echoed back.

// MaxModelName is the longest model name, in characters.
const MaxModelName = 200

// Limits on an endpoint's own settings.
const (
	// MinContextTokens is the least context window an Ollama server may load
	// its models with: a draft's floor in the agent's probe
	// (agent_server::probe::context_floor), its system prompt and tools with
	// room for its turns. Every setting drafts in its one window, so a smaller
	// one could only fail its test, and a local install's default, which no
	// probe tests, could only fail drafts.
	MinContextTokens    = 65536
	MaxContextTokens    = 1 << 20
	MaxCallsInFlightCap = 64
)

// Rules are what a deployment allows of a provider's endpoint.
type Rules struct {
	// AllowPrivate lets endpoints be http and private addresses: local mode,
	// where the model server may be on the same machine.
	AllowPrivate bool
}

// Invalid is a provider the rules refuse: Field names what to fix.
type Invalid struct {
	Field  string
	Reason string
}

func (e *Invalid) Error() string { return e.Field + " " + e.Reason }

func invalid(field, reason string) error { return &Invalid{Field: field, Reason: reason} }

// ErrPrivateHost is an endpoint whose host resolves to a private address.
var ErrPrivateHost = errors.New("the endpoint resolves to a private address")

var (
	// awsRegion is the agent's models::AwsRegion, which the Bedrock runtime's
	// host is built from.
	awsRegion = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-\d+$`)
	// azureHosts are the hosts an Azure OpenAI resource answers at.
	azureHosts = []string{".openai.azure.com", ".cognitiveservices.azure.com", ".services.ai.azure.com"}
)

// Validate checks the settings' shape: the provider's fields under r, every
// model's name and its price for the kind. Prices that name the catalog are
// Catalog.Unpriced's to check.
func (s Settings) Validate(r Rules) error {
	switch p := s.Provider.(type) {
	case Anthropic, OpenAI:
	case AzureOpenAI:
		if _, err := AzureEndpoint(p.Endpoint); err != nil {
			return err
		}
	case Bedrock:
		if !awsRegion.MatchString(p.Region) {
			return invalid("provider.region", "must be an AWS region, like us-east-1")
		}
	case Ollama:
		if _, err := OllamaURL(p.BaseURL, r); err != nil {
			return err
		}
		if p.ContextTokens < MinContextTokens || p.ContextTokens > MaxContextTokens {
			return invalid("provider.context_tokens", fmt.Sprintf("must be %d to %d", MinContextTokens, MaxContextTokens))
		}
		// Helix's price table has no Ollama rows: a model there is free, or
		// costs what its owner says it does.
		for _, rm := range s.Models.Each() {
			if _, table := rm.Model.Price.(PriceTable); table {
				return invalid("models."+string(rm.Role)+".price", "must be free or your own rates for a model on an Ollama server")
			}
		}
	case Compatible:
		if _, err := ModelURL(p.BaseURL, r); err != nil {
			return err
		}
		if p.API != CompatChatCompletions && p.API != CompatResponses {
			return invalid("provider.api", "must be chat_completions or responses")
		}
		if p.MaxCallsInFlight == 0 || p.MaxCallsInFlight > MaxCallsInFlightCap {
			return invalid("provider.max_calls_in_flight", fmt.Sprintf("must be 1 to %d", MaxCallsInFlightCap))
		}
	case nil:
		return invalid("provider", "is required")
	default:
		return invalid("provider", "is not a known kind")
	}
	kind := s.Provider.Kind()
	for _, rm := range s.Models.Each() {
		field := "models." + string(rm.Role)
		if err := ModelName(rm.Model.Name); err != nil {
			return invalid(field+".name", err.Error())
		}
		switch p := rm.Model.Price.(type) {
		case PriceTable:
		case PriceTableAs:
			if kind != KindAzureOpenAI && kind != KindBedrock {
				return invalid(field+".price", "can be another model's only for an Azure deployment or a Bedrock inference profile")
			}
			if err := ModelName(p.Model); err != nil {
				return invalid(field+".price.table_as", err.Error())
			}
		case PriceRates:
			if p.Output == 0 {
				return invalid(field+".price.rates.output", "must be more than 0")
			}
		case PriceFree:
			if !kind.Free() {
				return invalid(field+".price", "can be free only for Ollama and OpenAI-compatible servers")
			}
		default:
			return invalid(field+".price", "is required")
		}
	}
	return nil
}

// keyPrefixes start providers' API keys and no model's name: Anthropic's
// and OpenAI's sk-, Bedrock's long-term ABSK and short-term
// bedrock-api-key-.
var keyPrefixes = []string{"sk-", "ABSK", "bedrock-api-key-"}

// ModelName checks a model name: 1 to MaxModelName printable characters, no
// whitespace, and not an API key pasted into the wrong field.
func ModelName(name string) error {
	switch n := utf8.RuneCountInString(name); {
	case n == 0:
		return errors.New("is required")
	case n > MaxModelName || !utf8.ValidString(name):
		return fmt.Errorf("must be at most %d characters", MaxModelName)
	case strings.ContainsFunc(name, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }):
		return errors.New("must be printable, with no spaces")
	case slices.ContainsFunc(keyPrefixes, func(prefix string) bool { return strings.HasPrefix(name, prefix) }):
		return errors.New("looks like an API key; enter the key in the API key field")
	}
	return nil
}

// OllamaURL is an Ollama server's address under r, normalized as ModelURL
// does: the agent calls {base}/api/chat, so an OpenAI-compatible /v1, or the
// API's own path, is a mistake every call would 404 on.
//
//	OllamaURL("http://127.0.0.1:11434/", Rules{AllowPrivate: true}) // "http://127.0.0.1:11434"
func OllamaURL(raw string, r Rules) (string, error) {
	base, err := ModelURL(raw, r)
	if err != nil {
		return "", err
	}
	if u, _ := url.Parse(base); strings.HasSuffix(u.Path, "/api") || strings.HasSuffix(u.Path, "/v1") || strings.Contains(u.Path, "/api/") {
		return "", invalid("provider.base_url", "must be the Ollama server's address, like http://127.0.0.1:11434, without /api or /v1")
	}
	return base, nil
}

// ModelURL is an Ollama or OpenAI-compatible base URL in its normal form, the
// one stored and compared for key reuse: http or https, with a host, maybe a
// port and a path (an OpenAI-compatible server's /v1 included), and no user,
// query or fragment, as the agent's ModelUrl takes one. Normal is the scheme
// and host lowercased (an IP address written as netip writes it), the
// scheme's default port dropped, and the path as typed, its escapes kept,
// without trailing slashes: HTTPS://LLM.Example.com:443/v1/ is
// https://llm.example.com/v1.
//
// A host is an IP address or a name of letters, digits, hyphens and
// underscores, so the agent dials the host checked here: never a name it
// would read as an address in another form (2130706433, 0x7f.1). Under r
// without AllowPrivate it must be https and its host no private address
// literal, as the agent's guard counts them (a superset of the connector
// guard's); a name that resolves to one is refused by CheckHosts.
func ModelURL(raw string, r Rules) (string, error) {
	const field = "provider.base_url"
	u, err := url.Parse(raw)
	switch {
	case err != nil || (u.Scheme != "https" && u.Scheme != "http") || !modelHost(u):
		return "", invalid(field, "must be an http or https URL with a host")
	case u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#"):
		return "", invalid(field, "must have no user, query or fragment")
	case u.Scheme == "http" && !r.AllowPrivate:
		return "", invalid(field, "must be https")
	}
	host := strings.ToLower(u.Hostname())
	// A literal skips the resolver, so it is checked here; a name, by CheckHosts.
	if addr, err := netip.ParseAddr(host); err == nil {
		if !r.AllowPrivate && agentPrivate(addr) {
			return "", ErrPrivateHost
		}
		host = addr.String()
		if addr.Is6() {
			host = "[" + host + "]"
		}
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", invalid(field, "must have a port from 1 to 65535")
		}
		if n != map[string]int{"http": 80, "https": 443}[u.Scheme] {
			host += ":" + strconv.Itoa(n)
		}
	}
	return u.Scheme + "://" + host + strings.TrimRight(u.EscapedPath(), "/"), nil
}

// modelHost reports whether u's host is one the agent's URL parser reads as
// Go's does: an IP address with no zone, bracketed only if IPv6, or a DNS
// name of ASCII letters, digits, hyphens and underscores whose last label is
// not a number (the WHATWG parser reads such a name as an IPv4 address).
func modelHost(u *url.URL) bool {
	host := u.Hostname()
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Zone() == "" && addr.Is6() == strings.HasPrefix(u.Host, "[")
	}
	labels := strings.Split(strings.TrimSuffix(host, "."), ".")
	for _, label := range labels {
		if label == "" || strings.ContainsFunc(label, func(r rune) bool {
			return r >= utf8.RuneSelf || (r != '-' && r != '_' && !unicode.IsLetter(r) && !unicode.IsDigit(r))
		}) {
			return false
		}
	}
	last := strings.ToLower(labels[len(labels)-1])
	hex, isHex := strings.CutPrefix(last, "0x")
	number := strings.Trim(last, "0123456789") == "" || (isHex && strings.Trim(hex, "0123456789abcdef") == "")
	return !number
}

// agentPrivateV4 and agentPrivateV6 are the ranges the agent's guard
// (llm::guard::is_private) refuses, besides IPv6 outside global unicast and
// the IPv4 addresses NAT64 and 6to4 reach: an endpoint at one would save,
// and every call to it be refused.
var (
	agentPrivateV4 = prefixes("0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4")
	agentPrivateV6 = prefixes("2001::/32", "2001:2::/48", "2001:db8::/32", "3fff::/20")
	nat64          = netip.MustParsePrefix("64:ff9b::/96")
)

func prefixes(ranges ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(ranges))
	for i, r := range ranges {
		out[i] = netip.MustParsePrefix(r)
	}
	return out
}

// agentPrivate reports whether the agent refuses to call addr unless it
// allows private model hosts: its private, shared, reserved and
// documentation IPv4 ranges; the IPv4 address a NAT64 or 6to4 address
// reaches; Teredo, benchmarking and documentation IPv6; and anything outside
// global unicast, 2000::/3, IPv4-mapped addresses among them.
func agentPrivate(addr netip.Addr) bool {
	contains := func(p netip.Prefix) bool { return p.Contains(addr) }
	if addr.Is4() {
		return slices.ContainsFunc(agentPrivateV4, contains)
	}
	b := addr.As16()
	switch {
	case nat64.Contains(addr):
		return agentPrivate(netip.AddrFrom4([4]byte(b[12:16])))
	case b[0] == 0x20 && b[1] == 0x02:
		return agentPrivate(netip.AddrFrom4([4]byte(b[2:6])))
	}
	return slices.ContainsFunc(agentPrivateV6, contains) || b[0]&0xe0 != 0x20
}

// AzureEndpoint is an Azure OpenAI resource's endpoint, normalized to
// https://<host>/openai/v1: https on an Azure OpenAI host, with no port,
// user, query or fragment, and no path but /openai/v1.
func AzureEndpoint(raw string) (string, error) {
	const field = "provider.endpoint"
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Port() != "" || u.User != nil ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", invalid(field, "must be https://<resource>.openai.azure.com")
	}
	host := strings.ToLower(u.Hostname())
	if !hasAzureSuffix(host) {
		return "", invalid(field, "must be an Azure OpenAI endpoint, like https://<resource>.openai.azure.com")
	}
	if path := strings.TrimRight(u.Path, "/"); path != "" && path != "/openai/v1" {
		return "", invalid(field, "must have no path but /openai/v1")
	}
	return "https://" + host + "/openai/v1", nil
}

func hasAzureSuffix(host string) bool {
	for _, suffix := range azureHosts {
		if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
			return true
		}
	}
	return false
}

// Normalized is the settings with their endpoint's URL normalized; they
// must have passed Validate under r.
func (s Settings) Normalized(r Rules) Settings {
	switch p := s.Provider.(type) {
	case AzureOpenAI:
		p.Endpoint, _ = AzureEndpoint(p.Endpoint)
		s.Provider = p
	case Ollama:
		p.BaseURL, _ = ModelURL(p.BaseURL, r)
		s.Provider = p
	case Compatible:
		p.BaseURL, _ = ModelURL(p.BaseURL, r)
		s.Provider = p
	}
	return s
}

// Hosts are the hosts the owner typed, which calls to the provider dial.
func (s Settings) Hosts() []string {
	var raw string
	switch p := s.Provider.(type) {
	case AzureOpenAI:
		raw = p.Endpoint
	case Ollama:
		raw = p.BaseURL
	case Compatible:
		raw = p.BaseURL
	default:
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return nil
	}
	return []string{u.Hostname()}
}

// CheckHosts resolves the hosts the owner typed through guard, and returns
// ErrPrivateHost when one resolves to an address the guard refuses.
func (s Settings) CheckHosts(ctx context.Context, guard connector.Guard) error {
	for _, host := range s.Hosts() {
		_, err := guard.Resolve(ctx, host)
		switch {
		case errors.Is(err, connector.ErrPrivateAddress):
			return ErrPrivateHost
		case err != nil:
			return invalid("provider", "names a host that does not resolve")
		}
	}
	return nil
}
