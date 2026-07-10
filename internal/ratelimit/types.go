package ratelimit

// The structs in this file mirror the JSON wire format documented in
// docs/cn/plugin. Field names and shapes are taken verbatim from:
//   - scheduler.md            (SchedulerPickRequest/Response, Candidate)
//   - response-interceptor.md (ResponseInterceptRequest/Response)
//   - response-stream-interceptor.md (StreamChunkInterceptRequest/Response)
//   - usage-plugin.md         (UsageRecord, UsageFailure)
// They carry json tags so the SDK glue can (un)marshal them directly.

// SchedulerOptions is the Options sub-object of a scheduler request.
type SchedulerOptions struct {
	Headers  map[string][]string `json:"Headers,omitempty"`
	Metadata map[string]any      `json:"Metadata,omitempty"`
}

// Candidate is a single credential record offered to the scheduler.
type Candidate struct {
	ID         string         `json:"ID"`
	Provider   string         `json:"Provider"`
	Priority   int            `json:"Priority"`
	Status     string         `json:"Status"`
	Attributes map[string]any `json:"Attributes,omitempty"`
	Metadata   map[string]any `json:"Metadata,omitempty"`
}

// SchedulerPickRequest is the scheduler.pick request payload.
type SchedulerPickRequest struct {
	Provider   string           `json:"Provider"`
	Providers  []string         `json:"Providers,omitempty"`
	Model      string           `json:"Model"`
	Stream     bool             `json:"Stream"`
	Options    SchedulerOptions `json:"Options"`
	Candidates []Candidate      `json:"Candidates"`
}

// SchedulerPickResponse is the scheduler.pick result.
//
//   - Handled=true + AuthID:  pick this credential.
//   - Handled=true + DelegateBuiltin: defer to the named built-in scheduler
//     ("round-robin" or "fill-first").
//   - Handled=false: do not handle this scheduling decision.
type SchedulerPickResponse struct {
	AuthID          string `json:"AuthID,omitempty"`
	DelegateBuiltin string `json:"DelegateBuiltin,omitempty"`
	Handled         bool   `json:"Handled"`
}

// ResponseInterceptRequest is the response.intercept_after request payload.
type ResponseInterceptRequest struct {
	SourceFormat    string              `json:"SourceFormat"`
	Model           string              `json:"Model"`
	RequestedModel  string              `json:"RequestedModel"`
	Stream          bool                `json:"Stream"`
	RequestHeaders  map[string][]string `json:"RequestHeaders,omitempty"`
	ResponseHeaders map[string][]string `json:"ResponseHeaders,omitempty"`
	OriginalRequest string              `json:"OriginalRequest,omitempty"`
	RequestBody     string              `json:"RequestBody,omitempty"`
	Body            string              `json:"Body,omitempty"`
	StatusCode      int                 `json:"StatusCode"`
	Metadata        map[string]any      `json:"Metadata,omitempty"`
}

// ResponseInterceptResponse is the response.intercept_after result. Returning
// it zero-valued means "no changes".
type ResponseInterceptResponse struct {
	Headers      map[string][]string `json:"Headers,omitempty"`
	Body         string              `json:"Body,omitempty"`
	ClearHeaders []string            `json:"ClearHeaders,omitempty"`
}

// StreamChunkInterceptRequest is the response.intercept_stream_chunk payload.
// ChunkIndex == -1 is the header-only initialisation call.
type StreamChunkInterceptRequest struct {
	SourceFormat    string              `json:"SourceFormat"`
	Model           string              `json:"Model"`
	RequestedModel  string              `json:"RequestedModel"`
	RequestHeaders  map[string][]string `json:"RequestHeaders,omitempty"`
	ResponseHeaders map[string][]string `json:"ResponseHeaders,omitempty"`
	OriginalRequest string              `json:"OriginalRequest,omitempty"`
	RequestBody     string              `json:"RequestBody,omitempty"`
	Body            string              `json:"Body,omitempty"`
	HistoryChunks   []string            `json:"HistoryChunks,omitempty"`
	ChunkIndex      int                 `json:"ChunkIndex"`
	Metadata        map[string]any      `json:"Metadata,omitempty"`
}

// StreamChunkInterceptResponse is the stream chunk interception result.
type StreamChunkInterceptResponse struct {
	Headers      map[string][]string `json:"Headers,omitempty"`
	Body         string              `json:"Body,omitempty"`
	ClearHeaders []string            `json:"ClearHeaders,omitempty"`
	DropChunk    bool                `json:"DropChunk"`
}

// UsageRecord is the usage.handle payload.
type UsageRecord struct {
	Provider        string              `json:"Provider"`
	ExecutorType    string              `json:"ExecutorType"`
	Model           string              `json:"Model"`
	Alias           string              `json:"Alias,omitempty"`
	APIKey          string              `json:"APIKey,omitempty"`
	AuthID          string              `json:"AuthID,omitempty"`
	AuthIndex       string              `json:"AuthIndex,omitempty"`
	AuthType        string              `json:"AuthType,omitempty"`
	Source          string              `json:"Source,omitempty"`
	RequestedAt     string              `json:"RequestedAt,omitempty"`
	Latency         int64               `json:"Latency,omitempty"`
	TTFT            int64               `json:"TTFT,omitempty"`
	Failed          bool                `json:"Failed,omitempty"`
	Detail          map[string]any      `json:"Detail,omitempty"`
	ResponseHeaders map[string][]string `json:"ResponseHeaders,omitempty"`
	Failure         *UsageFailure       `json:"Failure,omitempty"`
}

// UsageFailure describes a failed request.
type UsageFailure struct {
	StatusCode int    `json:"StatusCode"`
	Body       string `json:"Body,omitempty"`
}
