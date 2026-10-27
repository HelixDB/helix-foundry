package ai

// CanReuseKey reports whether saving next with the key left blank keeps the
// active settings' key: only where the key still belongs to the endpoint the
// calls go to. An Anthropic, OpenAI or Bedrock key belongs to the account,
// whatever models are chosen; an Azure key to its resource and deployment;
// an OpenAI-compatible server's to its base URL in ModelURL's normal form
// (scheme and host lowercased, the default port and trailing slashes
// dropped, the path otherwise as typed), whatever its API, calls at once or
// models; Ollama takes none. Both must have passed Validate.
func CanReuseKey(active, next Settings) bool {
	if active.Provider == nil {
		return false
	}
	switch n := next.Provider.(type) {
	case Anthropic, OpenAI, Bedrock:
		return active.Provider.Kind() == n.Kind()
	case AzureOpenAI:
		a, ok := active.Provider.(AzureOpenAI)
		if !ok {
			return false
		}
		before, errBefore := AzureEndpoint(a.Endpoint)
		after, errAfter := AzureEndpoint(n.Endpoint)
		return errBefore == nil && errAfter == nil && before == after && active.Models.Draft.Name == next.Models.Draft.Name
	case Compatible:
		a, ok := active.Provider.(Compatible)
		if !ok {
			return false
		}
		// The rules only decide whether a URL is allowed, never its normal form.
		rules := Rules{AllowPrivate: true}
		before, errBefore := ModelURL(a.BaseURL, rules)
		after, errAfter := ModelURL(n.BaseURL, rules)
		return errBefore == nil && errAfter == nil && before == after
	default:
		return false
	}
}
