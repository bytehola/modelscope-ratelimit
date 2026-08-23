package ratelimit

import (
	"fmt"
	"strings"
)

// Config holds the plugin configuration. It is parsed from the host-provided
// plugins.configs.<pluginID> YAML block (the host strips its own "enabled" and
// "priority" fields and passes the rest to the plugin).
type Config struct {
	// Providers is the set of CLIProxyAPI provider IDs this plugin manages.
	// The scheduler only intercepts requests whose Provider is in this set;
	// everything else is left to the built-in scheduler (Handled=false).
	// Required for scheduling to take effect.
	//
	// Order defines the cross-provider rotation sequence: when
	// skip_provider_on_429 is OFF, a single request starts from the first
	// provider and advances to the next after each 429+insufficient_quota; any
	// 200 resets to the first provider so the next request starts there. Each
	// provider also round-robins its own keys independently (across requests).
	Providers []string `json:"providers" yaml:"providers"`

	// SkipProviderOn429 activates "skip-provider-on-429" mode: on a managed
	// 429+insufficient_quota, the provider is added to a latching skip set so
	// the next pick falls through to non-managed providers. Any 200 (managed
	// or non-managed) clears the set so the next request probes the managed
	// quota again. When OFF (default), a 429+insufficient_quota rotates the
	// within-request cross-provider cursor and applies the insufficient_quota
	// cooldown backoff instead; any 200 resets that cursor to the first
	// provider.
	SkipProviderOn429 bool `json:"skip_provider_on_429" yaml:"skip_provider_on_429"`

	// Timezone is the IANA timezone used for the daily 00:00 reset boundary.
	// Defaults to "Asia/Shanghai".
	Timezone string `json:"timezone" yaml:"timezone"`

	// CredentialStrategy controls how the plugin picks among available managed
	// keys within a provider: "round-robin" (default) spreads load; "fill-first"
	// always uses the first key until disabled. Only applies to managed providers.
	CredentialStrategy string `json:"credential_strategy" yaml:"credential_strategy"`

	// InsufficientQuotaCooldown is the base (in seconds) for the global
	// exponential-backoff cooldown applied after a 429+insufficient_quota.
	// Because Modelscope shares a quota across all managed keys, the cooldown
	// is global: SchedulerPick blocks all managed picks until the active
	// cooldown expires. Consecutive failures double the interval, capped at
	// 60 seconds, with no jitter; all keys in the same round expire together
	// (no drift). Any 200 resets it. Set to 0 to disable the cooldown entirely
	// (429 rotates to the next provider/key immediately, no blocking).
	// Supports fractional seconds (e.g. 0.1). Default 0.1.
	InsufficientQuotaCooldown float64 `json:"insufficient_quota_cooldown" yaml:"insufficient_quota_cooldown"`

	// MsProxyURL, when set, routes all Modelscope API calls through this proxy.
	// Used for debugging/traffic inspection (e.g. "http://127.0.0.1:8888").
	// TODO: remove after debugging is complete.
	MsProxyURL string `json:"ms_proxy_url" yaml:"ms_proxy_url"`

	// HostBaseURL is the absolute base URL of the CLIProxyAPI management API
	// (e.g. "http://127.0.0.1:8317"), used for server-side key resolution. When
	// set together with ManagementKey, the plugin resolves masked api-keys and
	// the real total server-side (direct net/http, no cgo callback) and bakes
	// them into the status page (no browser fetch, no key in the page).
	HostBaseURL string `json:"host_base_url" yaml:"host_base_url"`

	// ManagementKey is the CLIProxyAPI management key, read from the plugin
	// config block in config.yaml. It is used ONLY server-side (with net/http)
	// when HostBaseURL is set; it is never rendered into the (unauthenticated)
	// resource page. Storing it in config.yaml is the operator's responsibility;
	// restrict the file's permissions accordingly.
	ManagementKey string `json:"management_key" yaml:"management_key"`
}

// DefaultConfig returns the configuration tuned for the Modelscope
// api-inference.modelscope.cn/v1/chat/completions endpoint.
func DefaultConfig() *Config {
	return &Config{
		HostBaseURL:               "http://127.0.0.1:8317",
		Timezone:                  "Asia/Shanghai",
		CredentialStrategy:        "round-robin",
		InsufficientQuotaCooldown: 0.1,
	}
}

// openaiCompatProviderPrefix is the prefix the host prepends to an
// openai-compatibility provider name to form the runtime provider key
// (see CLIProxyAPI util.OpenAICompatibleProviderKey). "modelscope" becomes
// "openai-compatible-modelscope".
const openaiCompatProviderPrefix = "openai-compatible-"

// extractProviderName strips the known host prefixes from a provider key or
// auth ID to recover the bare provider name that the operator configured:
//
//   - "openai-compatible-<name>" (dash runtime key from the host scheduler)
//   - "openai-compatibility:<name>:<hash>" (colon auth-kind prefix / auth ID)
//   - "<name>" (bare configured name, passed verbatim)
//
// After stripping, the caller does an exact (case-insensitive) comparison
// against Config.Providers. This is unambiguous: "modelscope" and
// "modelscope-my" never collide, whereas the old token-based approach split
// "modelscope-my" on "-" and matched the "modelscope" token.
func extractProviderName(p string) string {
	if strings.HasPrefix(p, openaiCompatProviderPrefix) {
		return p[len(openaiCompatProviderPrefix):]
	}
	if strings.HasPrefix(p, "openai-compatibility:") {
		rest := p[len("openai-compatibility:"):]
		if idx := strings.IndexByte(rest, ':'); idx >= 0 {
			return rest[:idx]
		}
		return rest
	}
	return p
}

// ProviderIndex returns the position (0-based) of the configured provider that
// matches the given runtime provider key or auth ID, or -1 when no entry
// matches. The host generates keys in two fixed forms ("openai-compatible-<name>"
// for scheduler candidates and "openai-compatibility:<name>:<hash>" for auth
// IDs), so the prefix is stripped and the bare name is compared exactly
// (case-insensitive). This replaces the old token-based matching which could
// not distinguish "modelscope" from "modelscope-my" because "-" is a token
// separator.
func (c *Config) ProviderIndex(provider string) int {
	if len(c.Providers) == 0 {
		return -1
	}
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" {
		return -1
	}
	p = extractProviderName(p)
	for idx, want := range c.Providers {
		if strings.ToLower(strings.TrimSpace(want)) == p {
			return idx
		}
	}
	return -1
}

// ManagesProvider reports whether the given provider key is in scope. An empty
// Providers set means the plugin manages nothing.
func (c *Config) ManagesProvider(provider string) bool {
	return c.ProviderIndex(provider) >= 0
}

// ConfigFromMap builds a Config from a generic map (the glue parses the host
// YAML into a map and delegates here). Missing or zero-value fields fall back
// to DefaultConfig so partial configs stay valid across versions.
func ConfigFromMap(raw map[string]any) (*Config, error) {
	cfg := DefaultConfig()
	if raw == nil {
		return cfg, nil
	}
	if v, ok := raw["providers"]; ok {
		cfg.Providers = toStringSlice(v)
	}
	if v, ok := raw["skip_provider_on_429"].(bool); ok {
		cfg.SkipProviderOn429 = v
	}
	if v, ok := raw["host_base_url"].(string); ok && strings.TrimSpace(v) != "" {
		cfg.HostBaseURL = strings.TrimSpace(v)
	}
	if v, ok := raw["management_key"].(string); ok {
		cfg.ManagementKey = strings.TrimSpace(v)
	}
	if v, ok := raw["timezone"].(string); ok && v != "" {
		cfg.Timezone = v
	}
	if v, ok := raw["credential_strategy"].(string); ok {
		cfg.CredentialStrategy = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := raw["insufficient_quota_cooldown"]; ok {
		if n, err := toFloat64(v); err == nil {
			cfg.InsufficientQuotaCooldown = n
		} else {
			return nil, fmt.Errorf("invalid insufficient_quota_cooldown: %w", err)
		}
	}
	if v, ok := raw["ms_proxy_url"].(string); ok {
		cfg.MsProxyURL = strings.TrimSpace(v)
	}
	return cfg, nil
}

// toStringSlice coerces a YAML/JSON scalar or sequence into a []string.
func toStringSlice(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return append([]string(nil), t...)
	case string:
		if t == "" {
			return nil
		}
		return []string{t}
	default:
		return nil
	}
}

// toFloat64 coerces common YAML/JSON numeric representations into a float64.
func toFloat64(v any) (float64, error) {
	switch n := v.(type) {
	case int:
		return float64(n), nil
	case int64:
		return float64(n), nil
	case float64:
		return n, nil
	case string:
		var x float64
		_, err := fmt.Sscanf(n, "%f", &x)
		return x, err
	default:
		return 0, fmt.Errorf("unsupported numeric type %T", v)
	}
}
