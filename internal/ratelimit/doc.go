// Package ratelimit implements the Modelscope per-key, per-model and global
// daily rate-limit control for CLIProxyAPI.
//
// The package is intentionally free of any CLIProxyAPI SDK import so that the
// state machine, the daily 00:00 reset, the round-robin scheduler and the
// response-header parsing can be unit-tested in isolation. The thin SDK glue
// in cmd/plugin wires these helpers to the documented plugin hooks
// (scheduler.pick, response.intercept_after, response.intercept_stream_chunk
// and usage.handle).
//
// Business rules (see docs/cn/plugin):
//
//   - Parse the Modelscope rate-limit response headers:
//     Modelscope-Ratelimit-Model-Requests-Remaining  (per-model remaining)
//     Modelscope-Ratelimit-Requests-Remaining        (total remaining)
//   - When the per-model remaining is exhausted, disable only the current
//     model for the owning key for the rest of the day.
//   - When the total remaining is exhausted, disable the key globally (all
//     models) for the rest of the day.
//   - At 00:00 in the configured timezone the disable state is cleared and
//     the key (and its models) become eligible again.
//   - During scheduling, disabled key/model pairs are skipped and the request
//     is routed to the next available account instead of failing.
package ratelimit
