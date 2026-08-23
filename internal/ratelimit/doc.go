// Package ratelimit implements the Modelscope account-level magic-cube
// rate-limit control for CLIProxyAPI.
//
// The package is intentionally free of any CLIProxyAPI SDK import so that the
// state machine, the daily 00:00 reset, the round-robin scheduler and the
// response analysis can be unit-tested in isolation. The thin SDK glue in
// cmd/plugin wires these helpers to the documented plugin hooks
// (scheduler.pick, response.intercept_after, response.intercept_stream_chunk
// and usage.handle).
//
// Business rules:
//
//   - 429 + "insufficient balance" (magic cube exhausted): the key is
//     globally disabled for the rest of the day.
//   - 429 + "insufficient_quota" (shared pool rate window): a global
//     exponential-backoff cooldown blocks ALL managed scheduling for a short
//     interval (base seconds, doubling per consecutive failure, capped at 60).
//   - 401 Unauthorized: the key is globally disabled until plugin restart
//     (does NOT reset at midnight).
//   - At 00:00 in the configured timezone the daily disable state is cleared
//     and the key becomes eligible again.
//   - During scheduling, disabled keys are skipped and the request is routed
//     to the next available account instead of failing.
//   - When skip_provider_on_429 is OFF, a 429+insufficient_quota advances a
//     within-request cross-provider cursor so the host's retry picks the next
//     managed provider; any 200 resets that cursor to 0 so the next request
//     always starts from the first provider. Each provider keeps its own key
//     cursor, which keeps rotating across requests.
//   - When skip_provider_on_429 is ON, the 429'd provider is added to a
//     latching skip set so the next pick falls through to non-managed
//     providers until any 200 clears the set.
package ratelimit
