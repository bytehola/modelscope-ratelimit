package ratelimit

import "strings"

// PinnedProviderHeader is a plugin-internal marker. The Responses executor sets
// it on the nested host.model.execute call it uses for one provider's chat
// fallback, naming the single managed provider allowed to answer. scheduler.pick
// reads it back from Options.Headers.
//
// It is needed because the nested call routes a *shared* alias: if the preferred
// provider returns 429 inside that call, the host rotates to another managed
// credential on its own. That silently serves a Responses-enabled provider over
// the chat protocol and skips its own native step. The compat executor only sends
// auth-declared headers upstream, so this marker never leaves the process.
const PinnedProviderHeader = "X-Modelscope-Ratelimit-Provider"

// internalExecutionMetadataKey and internalExecutionSource mirror the host's
// modelExecutionMetadataSourceKey / modelExecutionInternalSource in
// CLIProxyAPI sdk/api/handlers/model_execution.go. They mark a request that came
// from a plugin's own host.model.execute callback instead of an external client.
const (
	internalExecutionMetadataKey = "source"
	internalExecutionSource      = "plugin_host_model_callback"
)

// PinnedProvider returns the managed provider that must serve this pick, or ""
// when the request is unconstrained.
//
// The pin is honoured only for internally originated requests: without that
// check any client could send the header on a normal request and dictate which
// account serves it.
func (c *Config) PinnedProvider(req SchedulerPickRequest) string {
	if c == nil || len(c.Providers) == 0 {
		return ""
	}
	if !isInternalExecution(req.Options.Metadata) {
		return ""
	}
	name := headerValue(req.Options.Headers, PinnedProviderHeader)
	if name == "" {
		return ""
	}
	if c.ProviderIndex(name) < 0 {
		// Not one of our managed providers: ignore the marker entirely.
		return ""
	}
	return name
}

// isInternalExecution reports whether the host flagged this request as coming
// from a plugin's nested model execution.
func isInternalExecution(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	raw, ok := meta[internalExecutionMetadataKey]
	if !ok {
		return false
	}
	value, okString := raw.(string)
	return okString && value == internalExecutionSource
}

// headerValue looks one header up case-insensitively, matching the tolerant
// lookup the rest of the payload parsing already uses.
func headerValue(headers map[string][]string, name string) string {
	for key, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(key), name) {
			continue
		}
		for _, value := range values {
			if trimmed := strings.ToLower(strings.TrimSpace(value)); trimmed != "" {
				return trimmed
			}
		}
		return ""
	}
	return ""
}
