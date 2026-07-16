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
	// Order determines preference: candidates from the first-listed provider are
	// always picked before the second, and so on. Only when every key from an
	// earlier provider is disabled (daily rate-limit exhausted) does the plugin
	// fall through to the next provider in the list.
	Providers []string `json:"providers" yaml:"providers"`

	// Timezone is the IANA timezone used for the daily 00:00 reset boundary.
	// Defaults to "Asia/Shanghai".
	Timezone string `json:"timezone" yaml:"timezone"`

	// Rate-limit response header names. Defaults target the Modelscope
	// api-inference.modelscope.cn endpoint.
	ModelLimitHeader     string `json:"model_limit_header" yaml:"model_limit_header"`
	ModelRemainingHeader string `json:"model_remaining_header" yaml:"model_remaining_header"`
	TotalLimitHeader     string `json:"total_limit_header" yaml:"total_limit_header"`
	TotalRemainingHeader string `json:"total_remaining_header" yaml:"total_remaining_header"`

	// DisableThreshold: a credential is disabled for the rest of the day when
	// its remaining count is <= this value. Default 0 means "exhausted".
	DisableThreshold int `json:"disable_threshold" yaml:"disable_threshold"`

	// CredentialStrategy controls how the plugin picks among available managed
	// keys within a provider: "round-robin" (default) spreads load; "fill-first"
	// always uses the first key until disabled. Only applies to managed providers.
	CredentialStrategy string `json:"credential_strategy" yaml:"credential_strategy"`

	// InsufficientQuotaCooldown is the number of seconds to temporarily cool
	// down (skip) a managed key after it returns HTTP 429 with an
	// "insufficient_quota" error body (e.g. Aliyun Model Studio quota
	// exhaustion). During the cooldown the scheduler picks a different key;
	// after it expires the key becomes available again. Default 10 seconds.
	InsufficientQuotaCooldown int `json:"insufficient_quota_cooldown" yaml:"insufficient_quota_cooldown"`

	// ProxyURL, when non-empty, activates proxy mode: on the first 429 from a
	// managed provider, the plugin waits 2s, probes the proxy by requesting
	// https://api-inference.modelscope.cn (10s timeout), and if reachable
	// enables the global upstream proxy via the management API. The proxy
	// stays on until a managed provider succeeds or all managed keys are
	// exhausted, then is disabled. When proxy mode is active,
	// InsufficientQuotaCooldown is ignored. Format: a single proxy URL such
	// as "socks5://user:pass@host:port" or "http://host:port".
	ProxyURL string `json:"proxy_url" yaml:"proxy_url"`

	// ManagedModels, if non-empty, restricts monitoring to these model names.
	// Empty means monitor every model served by the configured providers.
	ManagedModels []string `json:"managed_models" yaml:"managed_models"`

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
		ModelLimitHeader:          "Modelscope-Ratelimit-Model-Requests-Limit",
		ModelRemainingHeader:      "Modelscope-Ratelimit-Model-Requests-Remaining",
		TotalLimitHeader:          "Modelscope-Ratelimit-Requests-Limit",
		TotalRemainingHeader:      "Modelscope-Ratelimit-Requests-Remaining",
		DisableThreshold:          0,
		CredentialStrategy:        "round-robin",
		InsufficientQuotaCooldown: 10,
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

// ManagesModel reports whether the given model is in scope. When ManagedModels
// is empty every model is in scope.
func (c *Config) ManagesModel(model string) bool {
	if len(c.ManagedModels) == 0 {
		return true
	}
	for _, m := range c.ManagedModels {
		if m == model {
			return true
		}
	}
	return false
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
	if v, ok := raw["host_base_url"].(string); ok && strings.TrimSpace(v) != "" {
		cfg.HostBaseURL = strings.TrimSpace(v)
	}
	if v, ok := raw["management_key"].(string); ok {
		cfg.ManagementKey = strings.TrimSpace(v)
	}
	if v, ok := raw["managed_models"]; ok {
		cfg.ManagedModels = toStringSlice(v)
	}
	if v, ok := raw["timezone"].(string); ok && v != "" {
		cfg.Timezone = v
	}
	if v, ok := raw["model_limit_header"].(string); ok && v != "" {
		cfg.ModelLimitHeader = v
	}
	if v, ok := raw["model_remaining_header"].(string); ok && v != "" {
		cfg.ModelRemainingHeader = v
	}
	if v, ok := raw["total_limit_header"].(string); ok && v != "" {
		cfg.TotalLimitHeader = v
	}
	if v, ok := raw["total_remaining_header"].(string); ok && v != "" {
		cfg.TotalRemainingHeader = v
	}
	if v, ok := raw["disable_threshold"]; ok {
		if n, err := toInt(v); err == nil {
			cfg.DisableThreshold = n
		} else {
			return nil, fmt.Errorf("invalid disable_threshold: %w", err)
		}
	}
	if v, ok := raw["credential_strategy"].(string); ok {
		cfg.CredentialStrategy = strings.ToLower(strings.TrimSpace(v))
	}
	if v, ok := raw["insufficient_quota_cooldown"]; ok {
		if n, err := toInt(v); err == nil {
			cfg.InsufficientQuotaCooldown = n
		} else {
			return nil, fmt.Errorf("invalid insufficient_quota_cooldown: %w", err)
		}
	}
	if v, ok := raw["proxy_url"].(string); ok {
		cfg.ProxyURL = strings.TrimSpace(v)
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

// toInt coerces common YAML/JSON numeric representations into an int.
func toInt(v any) (int, error) {
	switch n := v.(type) {
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		return int(n), nil
	case string:
		var x int
		_, err := fmt.Sscanf(n, "%d", &x)
		return x, err
	default:
		return 0, fmt.Errorf("unsupported numeric type %T", v)
	}
}
