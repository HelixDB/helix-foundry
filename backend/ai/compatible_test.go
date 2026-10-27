package ai

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/ontologyagent"
)

// compatible is a custom OpenAI-compatible server at base, its models those
// an owner typed, one priced at the server's rates and the rest free.
func compatible(base string) Settings {
	return Settings{Provider: Compatible{BaseURL: base, API: CompatChatCompletions, MaxCallsInFlight: DefaultCallsInFlight}, Models: Roles{
		Draft: Model{Name: "llama-4-maverick", Price: PriceRates{Input: 270, Output: 850}}, Ask: Model{Name: "llama-4-maverick", Price: PriceFree{}},
		Extract: Model{Name: "qwen3-32b", Price: PriceFree{}}, Verify: Model{Name: "qwen3-32b", Price: PriceFree{}},
	}}
}

func TestCompatibleURL(t *testing.T) {
	hosted, local := Rules{}, Rules{AllowPrivate: true}
	// want is the normal form stored, or the refusal: "private" for
	// ErrPrivateHost, else the start of the reason.
	for raw, tc := range map[string]struct {
		rules Rules
		want  string
	}{
		"https://llm.example.com/v1":                {hosted, "https://llm.example.com/v1"},
		"HTTPS://LLM.Example.com:443/v1/":           {hosted, "https://llm.example.com/v1"},
		"https://llm.example.com:8443/v1":           {hosted, "https://llm.example.com:8443/v1"},
		"https://llm.example.com:08443/v1":          {hosted, "https://llm.example.com:8443/v1"},
		"https://gateway.example.com/openai/v1//":   {hosted, "https://gateway.example.com/openai/v1"},
		"https://llm.example.com":                   {hosted, "https://llm.example.com"},
		"https://llm.example.com/V1":                {hosted, "https://llm.example.com/V1"},
		"https://llm.example.com/team%2Fa/v1":       {hosted, "https://llm.example.com/team%2Fa/v1"},
		"https://my_llm.example.com/v1":             {hosted, "https://my_llm.example.com/v1"},
		"https://20.0.0.1/v1":                       {hosted, "https://20.0.0.1/v1"},
		"https://[2606:4700:0:0:0:0:0:1111]:443/v1": {hosted, "https://[2606:4700::1111]/v1"},
		"http://llm.example.com/v1":                 {hosted, "must be https"},
		"https://10.0.0.5/v1":                       {hosted, "private"},
		"https://127.0.0.1:8000/v1":                 {hosted, "private"},
		"https://169.254.169.254/latest":            {hosted, "private"},
		"https://[::1]/v1":                          {hosted, "private"},
		"https://[fd00:ec2::254]/v1":                {hosted, "private"},
		// The agent refuses these, though the connector guard would not.
		"https://[::ffff:8.8.8.8]/v1": {hosted, "private"},
		"https://192.0.2.1/v1":        {hosted, "private"},
		"https://240.0.0.1/v1":        {hosted, "private"},
		"https://[64:ff9b::a00:5]/v1": {hosted, "private"},
		// Names the agent reads as IPv4 addresses, and hosts it reads otherwise.
		"https://2130706433/v1":                       {hosted, "must be an http or https URL"},
		"https://0x7f.1/v1":                           {hosted, "must be an http or https URL"},
		"https://127.1/v1":                            {hosted, "must be an http or https URL"},
		"https://llm.example.123/v1":                  {hosted, "must be an http or https URL"},
		"https://bücher.example/v1":                   {hosted, "must be an http or https URL"},
		"https://llm..example.com/v1":                 {hosted, "must be an http or https URL"},
		"https://[fe80::1%25en0]:8000/v1":             {local, "must be an http or https URL"},
		"https://[20.0.0.1]/v1":                       {local, "must be an http or https URL"},
		"https:///v1":                                 {local, "must be an http or https URL"},
		"llm.example.com/v1":                          {local, "must be an http or https URL"},
		"ftp://llm.example.com/v1":                    {local, "must be an http or https URL"},
		"":                                            {local, "must be an http or https URL"},
		"https://sk-sentinel@llm.example.com/v1":      {local, "must have no user"},
		"https://@llm.example.com/v1":                 {local, "must have no user"},
		"https://llm.example.com/v1?api-key=sentinel": {local, "must have no user"},
		"https://llm.example.com/v1?":                 {local, "must have no user"},
		"https://llm.example.com/v1#":                 {local, "must have no user"},
		"https://llm.example.com:0/v1":                {local, "must have a port"},
		"https://llm.example.com:65536/v1":            {local, "must have a port"},
		// Locally the server may be on this machine or its network.
		"http://host.docker.internal:1234/v1": {local, "http://host.docker.internal:1234/v1"},
		"http://127.0.0.1:8000/v1/":           {local, "http://127.0.0.1:8000/v1"},
		"http://localhost:11434/v1":           {local, "http://localhost:11434/v1"},
		"http://10.0.0.5/v1":                  {local, "http://10.0.0.5/v1"},
		"http://[::1]:8080/v1":                {local, "http://[::1]:8080/v1"},
		"http://vllm:80/v1":                   {local, "http://vllm/v1"},
		"http://2130706433/v1":                {local, "must be an http or https URL"},
	} {
		got, err := ModelURL(raw, tc.rules)
		var bad *Invalid
		switch {
		case err == nil && got != tc.want:
			t.Errorf("ModelURL(%q) = %q, want %q", raw, got, tc.want)
		case errors.Is(err, ErrPrivateHost) && tc.want != "private":
			t.Errorf("ModelURL(%q) is private, want %q", raw, tc.want)
		case errors.Is(err, ErrPrivateHost):
		case err != nil && (!errors.As(err, &bad) || bad.Field != "provider.base_url" || !strings.HasPrefix(bad.Reason, tc.want)):
			t.Errorf("ModelURL(%q) = %v, want a base_url refusal saying %q", raw, err, tc.want)
		case err != nil && strings.Contains(err.Error(), "sentinel"):
			t.Errorf("ModelURL(%q) echoes the URL: %v", raw, err)
		}
	}
}

// The agent refuses to call these addresses unless it allows private model
// hosts (llm::guard's own test lists them): a hosted save refuses them too,
// and every address the connector guard refuses.
func TestAgentPrivate(t *testing.T) {
	for ip, want := range map[string]bool{
		"127.0.0.1": true, "127.255.0.9": true, "10.0.0.1": true, "172.16.0.1": true, "172.31.255.255": true, "192.168.1.1": true,
		"169.254.169.254": true, "100.64.0.0": true, "100.127.255.255": true, "0.0.0.0": true, "0.1.2.3": true, "255.255.255.255": true,
		"224.0.0.1": true, "::": true, "::1": true, "fc00::1": true, "fd00:ec2::254": true, "fe80::1": true, "febf::1": true, "ff02::1": true,
		"::ffff:8.8.8.8": true, "192.0.0.1": true, "192.0.0.255": true, "198.18.0.1": true, "198.19.255.255": true, "192.0.2.1": true,
		"198.51.100.1": true, "203.0.113.1": true, "240.0.0.1": true, "254.255.255.255": true, "64:ff9b::a00:5": true, "64:ff9b::7f00:1": true,
		"64:ff9b::a9fe:a9fe": true, "64:ff9b::6440:1": true, "64:ff9b:1::808:808": true, "2002:a00:5::1": true, "2002:c0a8:101::": true,
		"2002:7f00:1::1": true, "::10.0.0.5": true, "::8.8.8.8": true, "::ffff:0:808:808": true, "2001:0:4136:e378:8000:63bf:3fff:fdd2": true,
		"2001:2::1": true, "2001:db8::1": true, "3fff::1": true, "3fff:fff::1": true, "100::1": true, "4000::1": true, "e000::1": true,
		"8.8.8.8": false, "100.63.255.255": false, "100.128.0.0": false, "172.32.0.1": false, "1.1.1.1": false, "2606:4700::1111": false,
		"192.0.1.1": false, "192.0.3.1": false, "198.17.255.255": false, "198.20.0.1": false, "223.255.255.255": false,
		"64:ff9b::808:808": false, "64:ff9b::101:101": false, "2002:808:808::1": false, "2001:1::1": false, "2001:3::1": false,
		"2001:4860:4860::8888": false, "2a00:1450::1": false, "3fff:1000::1": false,
	} {
		addr := netip.MustParseAddr(ip)
		if got := agentPrivate(addr); got != want {
			t.Errorf("agentPrivate(%s) = %v, want %v", ip, got, want)
		}
		if _, err := (connector.Guard{}).Resolve(context.Background(), ip); err != nil && !agentPrivate(addr) {
			t.Errorf("the connector guard refuses %s, which a hosted save would take", ip)
		}
	}
}

func TestCompatibleSettings(t *testing.T) {
	hosted, local := Rules{}, Rules{AllowPrivate: true}
	catalog, err := VendoredCatalog()
	if err != nil {
		t.Fatal(err)
	}
	with := func(mutate func(*Settings)) Settings {
		s := compatible("https://llm.example.com/v1")
		mutate(&s)
		return s
	}
	on := func(base string, api CompatAPI, calls uint16) Settings {
		s := compatible(base)
		s.Provider = Compatible{BaseURL: base, API: api, MaxCallsInFlight: calls}
		return s
	}
	field := func(err error) string {
		var bad *Invalid
		switch {
		case errors.As(err, &bad):
			return bad.Field
		case errors.Is(err, ErrPrivateHost):
			return "private"
		}
		return ""
	}

	// The owner names the server's models and prices them at its rates, or
	// counts no cost; a server is sent at most 1 to 64 calls at once, over
	// Chat Completions or Responses.
	const base = "https://llm.example.com/v1"
	for name, tc := range map[string]struct {
		settings Settings
		rules    Rules
		want     string
	}{
		"rates and free":           {compatible(base), hosted, ""},
		"Responses":                {on(base, CompatResponses, 4), hosted, ""},
		"one call at once":         {on(base, CompatResponses, 1), hosted, ""},
		"64 calls at once":         {on(base, CompatResponses, 64), hosted, ""},
		"65 calls at once":         {on(base, CompatResponses, 65), hosted, "provider.max_calls_in_flight"},
		"no API":                   {on(base, "", 4), hosted, "provider.api"},
		"another model's price":    {with(func(s *Settings) { s.Models.Verify.Price = PriceTableAs{Model: "gpt-6.1-sol"} }), hosted, "models.verify.price"},
		"no output rate":           {with(func(s *Settings) { s.Models.Draft.Price = PriceRates{Input: 270} }), hosted, "models.draft.price.rates.output"},
		"a private address":        {on("https://10.1.2.3/v1", CompatResponses, 4), hosted, "private"},
		"a private address, local": {on("http://10.1.2.3/v1", CompatResponses, 4), local, ""},
		"a model with no name":     {with(func(s *Settings) { s.Models.Extract.Name = "" }), local, "models.extract.name"},
	} {
		if got := field(tc.settings.Validate(tc.rules)); got != tc.want {
			t.Errorf("%s: Validate() refused %q, want %q", name, got, tc.want)
		}
	}

	// Helix's table has no rows for a custom server, even for a model named
	// as one of OpenAI's: a model priced by the table is refused at save, as
	// the agent would refuse it unpriced.
	t.Run("prices no model by Helix's table", func(t *testing.T) {
		s := with(func(s *Settings) {
			s.Models.Draft.Price, s.Models.Verify = PriceTable{}, Model{Name: "gpt-6.1-sol", Price: PriceTable{}}
		})
		if err := s.Validate(hosted); err != nil {
			t.Fatal(err)
		}
		if got := catalog.Unpriced(s); len(got) != 2 || got[0] != RoleDraft || got[1] != RoleVerify {
			t.Fatalf("Unpriced() = %v", got)
		}
		if got := catalog.Unpriced(compatible("https://llm.example.com/v1")); len(got) != 0 {
			t.Fatalf("Unpriced() of rates and free = %v", got)
		}
	})

	// The owner types every model's name: the kind suggests none.
	t.Run("suggests no models", func(t *testing.T) {
		if roles, ok := Suggested(KindCompatible); ok || roles != (Roles{}) {
			t.Fatalf("Suggested() = %+v, %v", roles, ok)
		}
		if KindCompatible.Key() != KeyOptional || !KindCompatible.Free() {
			t.Fatalf("key %v, free %v", KindCompatible.Key(), KindCompatible.Free())
		}
	})

	// The base URL is stored in its normal form, the one key reuse compares.
	t.Run("stores the normal form", func(t *testing.T) {
		s := with(func(s *Settings) {
			s.Provider = Compatible{BaseURL: "HTTP://Host.Docker.Internal:80/v1/", API: CompatResponses, MaxCallsInFlight: 2}
		})
		if got := s.Normalized(local).Provider; got != (Compatible{BaseURL: "http://host.docker.internal/v1", API: CompatResponses, MaxCallsInFlight: 2}) {
			t.Fatalf("Normalized() = %+v", got)
		}
		if hosts := s.Normalized(local).Hosts(); len(hosts) != 1 || hosts[0] != "host.docker.internal" {
			t.Fatalf("Hosts() = %v", hosts)
		}
	})
}

func TestCompatibleKeyReuse(t *testing.T) {
	at := func(base string, mutate ...func(*Compatible)) Settings {
		s := compatible(base)
		p := s.Provider.(Compatible)
		for _, m := range mutate {
			m(&p)
		}
		s.Provider = p
		return s
	}
	active := at("https://llm.example.com/v1")
	// A blank key keeps the stored one for the same server: its base URL in
	// normal form, whatever the API, calls at once or models; any other URL,
	// path or port, or another kind, needs its own key.
	for name, tc := range map[string]struct {
		next Settings
		want bool
	}{
		"the same URL":             {at("https://llm.example.com/v1"), true},
		"the same URL, typed anew": {at("HTTPS://LLM.example.com:443/v1/"), true},
		"another API":              {at("https://llm.example.com/v1", func(p *Compatible) { p.API = CompatResponses }), true},
		"more calls at once":       {at("https://llm.example.com/v1", func(p *Compatible) { p.MaxCallsInFlight = 16 }), true},
		"other models": {func() Settings {
			s := at("https://llm.example.com/v1")
			s.Models.Draft.Name = "deepseek-v4"
			return s
		}(), true},
		"another path":     {at("https://llm.example.com/openai/v1"), false},
		"a path's case":    {at("https://llm.example.com/V1"), false},
		"another port":     {at("https://llm.example.com:8443/v1"), false},
		"plain http":       {at("http://llm.example.com/v1"), false},
		"another host":     {at("https://evil.example.com/v1"), false},
		"a subdomain":      {at("https://eu.llm.example.com/v1"), false},
		"OpenAI":           {Settings{Provider: OpenAI{}}, false},
		"Ollama, same URL": {Settings{Provider: Ollama{BaseURL: "https://llm.example.com/v1", ContextTokens: 32768}}, false},
	} {
		if got := CanReuseKey(active, tc.next); got != tc.want {
			t.Errorf("%s: CanReuseKey() = %v, want %v", name, got, tc.want)
		}
	}
	if CanReuseKey(Settings{Provider: OpenAI{}}, at("https://api.openai.com/v1")) {
		t.Error("an OpenAI key went to a custom server at OpenAI's URL")
	}
}

func TestCompatibleModels(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	cfg.Host.Local, cfg.Foundry.AI.AllowPrivateHosts, cfg.Foundry.AI.CallTimeout = true, true, 30*time.Minute
	local, err := NewDeployment(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	server := compatible("http://host.docker.internal:1234/v1")

	// A server's calls go to its base URL over its API, at most its calls at
	// once, each JSON or text call under the install's call timeout; its
	// rates and free models are priced as saved.
	t.Run("writes the endpoint and models", func(t *testing.T) {
		keys := keyring(t)
		c := savedConfig(t, keys, "cfg-c", "ws-1", "", server)
		c.ProbeReport = []byte(`{"checks":[{"role":"draft","passed":true,"reasoning":false,"seconds":4}]}`)
		got, err := (&Resolver{DB: activeConfigs{"ws-1": c}, Keyring: keys, Deployment: local}).Resolve(ctx, "ws-1")
		if err != nil {
			t.Fatal(err)
		}
		endpoint := ontologyagent.Endpoint{
			Provider:        ontologyagent.OpenAICompatible{BaseURL: "http://host.docker.internal:1234/v1", API: ontologyagent.CompatChatCompletions},
			CallTimeoutSecs: 1800, MaxCallsInFlight: DefaultCallsInFlight,
		}
		turn := got.Turn()
		// The draft model refused the reasoning knob at its probe, so it runs without.
		if turn.Endpoint != endpoint || !specEqual(turn.Draft, ontologyagent.ModelSpec{Name: "llama-4-maverick", Price: ontologyagent.PriceRates{Input: 270, Output: 850}}) ||
			turn.Extract != (ontologyagent.ModelSpec{Name: "qwen3-32b", Price: ontologyagent.PriceFree{}, Reasoning: true}) {
			t.Fatalf("Turn() = %+v", turn)
		}
		if ask := got.Ask(); ask.Endpoint != endpoint || ask.Ask != (ontologyagent.ModelSpec{Name: "llama-4-maverick", Price: ontologyagent.PriceFree{}, Reasoning: true}) {
			t.Fatalf("Ask() = %+v", ask)
		}
		if title := got.Title(); title.Endpoint != endpoint || title.Title != (ontologyagent.ModelSpec{Name: "qwen3-32b", Price: ontologyagent.PriceFree{}}) {
			t.Fatalf("Title() = %+v", title)
		}
		if !got.Key.IsZero() || got.Label() != "host.docker.internal" {
			t.Fatalf("key %v, label %q", got.Key, got.Label())
		}
	})

	// A key is optional: sent when saved, none otherwise; one that is stored
	// still needs the keyring and must open.
	t.Run("sends the key it was saved with", func(t *testing.T) {
		keys := keyring(t)
		keyed := savedConfig(t, keys, "cfg-k", "ws-k", "lm-studio-sentinel-0123456789", server)
		r := &Resolver{DB: activeConfigs{"ws-k": keyed}, Keyring: keys, Deployment: local}
		if got, err := r.Resolve(ctx, "ws-k"); err != nil || got.Key.Reveal() != "lm-studio-sentinel-0123456789" {
			t.Fatalf("Resolve() = %+v, %v", got, err)
		}
		r.Keyring = nil
		if _, err := r.Resolve(ctx, "ws-k"); !errors.Is(err, ErrNoKeyring) {
			t.Fatalf("no keyring: Resolve() = %v", err)
		}
		keyed.Secret.Ciphertext[0] ^= 0xff
		r.Keyring = keys
		if _, err := r.Resolve(ctx, "ws-k"); !errors.Is(err, ErrKeyUnreadable) {
			t.Fatalf("a key that won't open: Resolve() = %v", err)
		}
	})

	// Hosted, a custom server runs only on Pro or Max, and only on an agent
	// that calls the kind.
	t.Run("runs hosted on Pro, on an agent that calls it", func(t *testing.T) {
		hosted, err := NewDeployment(hostedConfig(sentinel))
		if err != nil {
			t.Fatal(err)
		}
		keys := keyring(t)
		c := savedConfig(t, keys, "cfg-h", "ws-1", "", compatible("https://llm.example.com/v1"))
		r := &Resolver{DB: activeConfigs{"ws-1": c}, Keyring: keys, Deployment: hosted, Plans: NewPlans(&onPlan{plan: freePlan})}
		if _, err := r.Resolve(ctx, "ws-1"); !errors.Is(err, ErrProRequired) {
			t.Fatalf("on Free: Resolve() = %v", err)
		}
		r.Plans = NewPlans(&onPlan{plan: proPlan})
		got, err := r.ResolveServed(ctx, "ws-1", []string{"anthropic", "openai_compatible"})
		if err != nil || got.Turn().Endpoint.CallTimeoutSecs != 600 || got.Label() != "llm.example.com" {
			t.Fatalf("on Pro: ResolveServed() = %+v, %v", got, err)
		}
		if _, err := r.ResolveServed(ctx, "ws-1", []string{"anthropic", "openai", "azure_openai"}); !errors.Is(err, ErrNotServed) {
			t.Fatalf("an agent that doesn't call it: ResolveServed() = %v", err)
		}
	})

	// The probe tests the server as its calls would run: every role with
	// reasoning on, the title without, on the same endpoint.
	t.Run("probes the server", func(t *testing.T) {
		probe := local.ProbeRequest("cfg-c", server, 15*time.Minute)
		want := ontologyagent.Endpoint{
			Provider:        ontologyagent.OpenAICompatible{BaseURL: "http://host.docker.internal:1234/v1", API: ontologyagent.CompatChatCompletions},
			CallTimeoutSecs: 1800, MaxCallsInFlight: DefaultCallsInFlight,
		}
		if probe.Endpoint != want || probe.Limits != (ontologyagent.ProbeLimits{MaxUSD: ProbeMaxUSD, MaxWallSecs: 900}) || len(probe.Checks) != 5 {
			t.Fatalf("probe = %+v", probe)
		}
		for _, c := range probe.Checks {
			if c.Model.Reasoning != (c.Role != ontologyagent.ProbeTitle) || c.Model.Revision != "" || c.Model.ContextTokens != 0 {
				t.Errorf("check %s = %+v", c.Role, c.Model)
			}
		}
		report := ReportOf(ontologyagent.ProbeResult{Checks: []ontologyagent.ProbeCheckResult{
			{Role: ontologyagent.ProbeDraft, Passed: true, Seconds: 12, Reasoning: false},
			{Role: ontologyagent.ProbeAsk, Passed: true, Seconds: 9, Reasoning: true},
		}})
		if report.Ollama != nil || report.Reasoning(RoleDraft) || !report.Reasoning(RoleAsk) || !report.Reasoning(RoleExtract) {
			t.Fatalf("report = %+v", report)
		}
	})
}

// A custom server's failures name it by its host, and send the owner to
// Settings for what only they can fix.
func TestCompatibleCopy(t *testing.T) {
	own := Resolved{Source: SourceWorkspace, Settings: compatible("https://llm.example.com/v1")}
	for _, tc := range []struct {
		name string
		got  Copy
		want Copy
	}{
		{"declined", DraftFailure(own, ontologyagent.ModelRefused), Copy{Message: "llm.example.com declined to draft this ontology", Permanent: true}},
		{"down", DraftFailure(own, ontologyagent.ModelUnavailable), Copy{Message: "llm.example.com is unavailable; Helix will retry"}},
		{"slow", DraftFailure(own, ontologyagent.ModelTimeout), Copy{Message: "llm.example.com is unavailable; Helix will retry"}},
		{"garbled", DraftFailure(own, ontologyagent.ModelMalformed), Copy{Message: "llm.example.com is unavailable; Helix will retry"}},
		{"its key", DraftFailure(own, ontologyagent.ModelAuth),
			Copy{Message: "llm.example.com refused the API key. An owner can enter a new one in Settings › AI models.", Permanent: true}},
		{"its model", DraftFailure(own, ontologyagent.ModelNotFound),
			Copy{Message: "llm.example.com has no model llama-4-maverick. An owner can choose another in Settings › AI models.", Permanent: true}},
		{"its quota", DraftFailure(own, ontologyagent.ModelQuota), Copy{Message: "The llm.example.com account is out of credit or quota.", Permanent: true}},
		{"no tools", DraftFailure(own, ontologyagent.ModelUnsupported),
			Copy{Message: "llama-4-maverick can’t do what this task needs (tool calls or structured answers).", Permanent: true}},
		{"too long", DraftFailure(own, ontologyagent.ModelContextTooLong), Copy{Message: "This was too long for llama-4-maverick.", Permanent: true}},
		{"moved inward", DraftFailure(own, ontologyagent.ModelPrivateHost), Copy{Message: "The endpoint resolves to a private address.", Permanent: true}},
		{"a question down", AskFailure(own, ontologyagent.ModelRateLimited), Copy{Message: askUnreachable}},
		{"a question's model", AskFailure(own, ontologyagent.ModelPermission),
			Copy{Message: "llm.example.com refused access to llama-4-maverick. An owner can choose another model in Settings › AI models.", Permanent: true}},
		{"a title", TitleFailure(own, ontologyagent.ModelAuth), Copy{}},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %+v, want %+v", tc.name, tc.got, tc.want)
		}
	}
	// A server every document would fail on stops the load, naming the
	// models documents are read with; a document's own trouble does not.
	if got, stop := DocumentFailure(own, ontologyagent.ModelPrivateHost); !stop || got.Message != "The endpoint resolves to a private address." {
		t.Errorf("a load moved inward = %+v, %v", got, stop)
	}
	if got, stop := DocumentFailure(own, ontologyagent.ModelNotFound); !stop ||
		got.Message != "llm.example.com has no model qwen3-32b. An owner can choose another in Settings › AI models." {
		t.Errorf("a load's model = %+v, %v", got, stop)
	}
	if got, stop := DocumentFailure(own, ontologyagent.ModelTruncated); stop || got != (Copy{}) {
		t.Errorf("a document's own trouble = %+v, %v", got, stop)
	}
	// A server at an address is named by it.
	if got := Label(Compatible{BaseURL: "http://[::1]:8000/v1"}); got != "::1" {
		t.Errorf("Label() = %q", got)
	}
}

func TestCompatibleDefault(t *testing.T) {
	// A local install's default may be a custom server, from .env: its base
	// URL, API (Chat Completions unless named), models and an optional key,
	// sent four calls at once, every model free.
	t.Run("reads a local install's server", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Host.Local, cfg.Foundry.AI.AllowPrivateHosts = true, true
		cfg.Foundry.AI.Kinds = []string{"anthropic", "openai_compatible"}
		cfg.Foundry.AI.Default = config.AIDefault{Provider: "openai_compatible", BaseURL: "http://Host.Docker.Internal:1234/v1/", Models: models("qwen3-32b")}
		d, err := NewDeployment(&cfg)
		if err != nil {
			t.Fatal(err)
		}
		want := Compatible{BaseURL: "http://host.docker.internal:1234/v1", API: CompatChatCompletions, MaxCallsInFlight: DefaultCallsInFlight}
		if d.Default.Settings.Provider != want || d.Default.Settings.Models != every(Model{Name: "qwen3-32b", Price: PriceFree{}}) ||
			!d.Default.Key.IsZero() || !d.Offers(KindCompatible) || Label(d.Default.Settings.Provider) != "host.docker.internal" {
			t.Fatalf("the default = %+v", d.Default.Settings)
		}
		cfg.Foundry.AI.Default.API, cfg.Foundry.AI.Default.APIKey = "responses", "sk-local-gateway-0123456789"
		if d, err = NewDeployment(&cfg); err != nil || d.Default.Settings.Provider.(Compatible).API != CompatResponses ||
			d.Default.Key.Reveal() != "sk-local-gateway-0123456789" {
			t.Fatalf("a keyed Responses default = %+v, %v", d, err)
		}
		r, err := (&Resolver{DB: activeConfigs{}, Deployment: d}).Resolve(context.Background(), "ws-1")
		if err != nil || r.Source != SourceDefault || r.Key.Reveal() != "sk-local-gateway-0123456789" || r.Turn().Endpoint.MaxCallsInFlight != DefaultCallsInFlight {
			t.Fatalf("Resolve() = %+v, %v", r, err)
		}
	})
	for name, def := range map[string]config.AIDefault{
		"no base URL":           {Provider: "openai_compatible", Models: models("m")},
		"a host read otherwise": {Provider: "openai_compatible", BaseURL: "http://0x7f.1:8000/v1", Models: models("m")},
		"an Azure endpoint":     {Provider: "openai_compatible", BaseURL: "https://llm.example.com/v1", Endpoint: "https://acme.openai.azure.com", Models: models("m")},
		"a region":              {Provider: "openai_compatible", BaseURL: "https://llm.example.com/v1", Region: "us-east-1", Models: models("m")},
		"a key as a model":      {Provider: "openai_compatible", BaseURL: "https://llm.example.com/v1", Models: models("sk-sentinel-0123456789")},
	} {
		cfg := hostedConfig(sentinel)
		local(def, true)(cfg)
		if _, err := NewDeployment(cfg); err == nil || strings.Contains(err.Error(), "sentinel") {
			t.Errorf("%s: NewDeployment() = %v", name, err)
		}
	}
}
