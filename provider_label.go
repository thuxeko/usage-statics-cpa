package main

// Provider display enrichment.
//
// Records reach us with the raw executor provider name, which for the
// openai-compatibility providers is an opaque slug ("openai-compatible-ca2")
// and for OAuth providers is a bare provider ("antigravity"). Neither tells
// the operator which upstream endpoint or which upstream account served the
// request.
//
// Two enrichments run at READ time so they apply to historical rows too:
//
//	base_url  map provider -> base-url from the CPA config.yaml
//	account   auth_id ("antigravity-<email>.json") -> the account email
//
// Both are best-effort: when the config is unreadable or the provider is not
// configured we fall back to the raw provider name rather than inventing a
// value. A wrong-but-plausible label is worse than an honest raw one.

import (
	"net/url"
	"strings"
	"sync"
)

const openAICompatPrefix = "openai-compatible-"

// baseURLIndex is rebuilt lazily and cached: the CPA config changes rarely and
// this is on the dashboard read path.
var (
	baseURLOnce  sync.Once
	baseURLByNam map[string]string
)

func configuredBaseURLs() map[string]string {
	baseURLOnce.Do(func() {
		baseURLByNam = map[string]string{}
		for _, p := range readCPAConfigProviders() {
			name := strings.TrimSpace(p.Name)
			base := strings.TrimSpace(p.BaseURL)
			if name == "" || base == "" {
				continue
			}
			baseURLByNam[name] = base
		}
	})
	return baseURLByNam
}

// invalidateBaseURLCache drops the cached config index so the next read
// re-parses config.yaml. Called when the provider list is refreshed.
func invalidateBaseURLCache() {
	baseURLOnce = sync.Once{}
	baseURLByNam = nil
}

// providerShortName strips the openai-compatibility slug prefix, yielding the
// operator-facing provider name ("openai-compatible-ca2" -> "ca2").
func providerShortName(provider string) string {
	p := strings.TrimSpace(provider)
	if strings.HasPrefix(p, openAICompatPrefix) {
		return strings.TrimSpace(strings.TrimPrefix(p, openAICompatPrefix))
	}
	return p
}

// baseURLForProvider resolves the upstream endpoint for a provider name.
func baseURLForProvider(provider string) string {
	short := providerShortName(provider)
	if short == "" {
		return ""
	}
	if v, ok := configuredBaseURLs()[short]; ok {
		return v
	}
	// Some configs key the entry by the full slug.
	if v, ok := configuredBaseURLs()[strings.TrimSpace(provider)]; ok {
		return v
	}
	return ""
}

// baseURLHost extracts a compact host ("llm.goaichat.top") from a base URL.
// Returns "" for values that carry no usable host so callers can fall back.
func baseURLHost(base string) string {
	raw := strings.TrimSpace(base)
	if raw == "" {
		return ""
	}
	candidate := raw
	if !strings.Contains(candidate, "://") {
		candidate = "http://" + candidate
	}
	parsed, err := url.Parse(candidate)
	if err != nil {
		return ""
	}
	host := strings.TrimSpace(parsed.Host)
	if host == "" {
		return ""
	}
	return host
}

// accountFromAuthID turns an auth identity into the upstream account it
// belongs to. OAuth providers identify the credential by file name
// ("antigravity-kevinhoang2334@gmail.com.json"), which already carries the
// account. It returns "" when the identity is empty or is not account-shaped
// (e.g. a synthesized "openai-compatibility:<name>:<hash>" key).
//
// provider is used only to strip the leading provider segment, so an account
// that itself contains a hyphen ("john-doe") is not truncated.
func accountFromAuthID(authID, provider string) string {
	id := strings.TrimSpace(authID)
	if id == "" {
		return ""
	}
	// Synthetic identities are "<provider>:<name>:<hash>" (note the "ity:"
	// spelling, which differs from the "openai-compatible-" provider prefix).
	if strings.HasPrefix(id, "openai-compatible") || strings.Contains(id, ":") {
		return ""
	}
	name := strings.TrimSpace(strings.TrimSuffix(id, ".json"))
	if name == "" {
		return ""
	}
	if short := providerShortName(provider); short != "" {
		if trimmed := strings.TrimSpace(strings.TrimPrefix(name, short+"-")); trimmed != name && trimmed != "" {
			return trimmed
		}
	}
	return name
}

// providerLabel builds the single operator-facing Provider cell.
//
//	openai-compatible-ca2  -> "ca2 · llm.goaichat.top"
//	antigravity            -> "kevinhoang2334@gmail.com"
//	unknown-provider       -> "unknown-provider"   (honest raw fallback)
//
// The raw provider name is always preserved in .Provider for filtering.
func providerLabel(detail RequestDetail) string {
	provider := strings.TrimSpace(detail.Provider)
	base := strings.TrimSpace(detail.BaseURL)
	if base == "" {
		base = baseURLForProvider(provider)
	}
	short := providerShortName(provider)
	host := baseURLHost(base)

	// OAuth accounts are the more specific identity: when we know which
	// upstream account served the request, that is the label.
	if account := accountFromAuthID(detail.AuthID, provider); account != "" {
		if short == "" || strings.EqualFold(short, "antigravity") {
			return account
		}
		return short + " · " + account
	}

	if host != "" {
		if short == "" {
			return host
		}
		return short + " · " + host
	}
	// No endpoint known: still prefer the friendly name over the raw slug.
	if short != "" {
		return short
	}
	return provider
}