package ratelimit

// 本文件的结构体逐字段镜像仓库 docs/cn/plugin/ 里约定的 JSON 线上格式，字段名与
// 形状均按原样取自：
//   - scheduler.md            （SchedulerPickRequest/Response、Candidate）
//   - response-interceptor.md （ResponseInterceptRequest/Response）
//   - response-stream-interceptor.md（StreamChunkInterceptRequest/Response）
//   - usage-plugin.md         （UsageRecord、UsageFailure）
// 结构体带 json tag，SDK 胶水层可以直接序列化/反序列化。

// SchedulerOptions 是 scheduler 请求里的 Options 子对象。
type SchedulerOptions struct {
	Headers  map[string][]string `json:"Headers,omitempty"`
	Metadata map[string]any      `json:"Metadata,omitempty"`
}

// Candidate 是宿主交给调度器的单条凭据记录。
type Candidate struct {
	ID         string         `json:"ID"`
	Provider   string         `json:"Provider"`
	Priority   int            `json:"Priority"`
	Status     string         `json:"Status"`
	Attributes map[string]any `json:"Attributes,omitempty"`
	Metadata   map[string]any `json:"Metadata,omitempty"`
}

// SchedulerPickRequest 是 scheduler.pick 的请求载荷。
type SchedulerPickRequest struct {
	Provider   string           `json:"Provider"`
	Providers  []string         `json:"Providers,omitempty"`
	Model      string           `json:"Model"`
	Stream     bool             `json:"Stream"`
	Options    SchedulerOptions `json:"Options"`
	Candidates []Candidate      `json:"Candidates"`
}

// SchedulerPickResponse 是 scheduler.pick 的返回结果。
//
//   - Handled=true + AuthID：使用该凭据。
//   - Handled=true + DelegateBuiltin：交给指定的内置调度器
//     （"round-robin" 或 "fill-first"）。
//   - Handled=false：本次调度不由插件决定。
type SchedulerPickResponse struct {
	AuthID          string `json:"AuthID,omitempty"`
	DelegateBuiltin string `json:"DelegateBuiltin,omitempty"`
	Handled         bool   `json:"Handled"`
}

// ResponseInterceptRequest 是 response.intercept_after 的请求载荷。
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

// ResponseInterceptResponse 是 response.intercept_after 的返回结果。
// 返回零值表示“不改动任何东西”。
type ResponseInterceptResponse struct {
	Headers      map[string][]string `json:"Headers,omitempty"`
	Body         string              `json:"Body,omitempty"`
	ClearHeaders []string            `json:"ClearHeaders,omitempty"`
}

// StreamChunkInterceptRequest 是 response.intercept_stream_chunk 的请求载荷。
// ChunkIndex == -1 表示仅带响应头的初始化调用。
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

// StreamChunkInterceptResponse 是分块流拦截的返回结果。
type StreamChunkInterceptResponse struct {
	Headers      map[string][]string `json:"Headers,omitempty"`
	Body         string              `json:"Body,omitempty"`
	ClearHeaders []string            `json:"ClearHeaders,omitempty"`
	DropChunk    bool                `json:"DropChunk"`
}

// UsageRecord 是 usage.handle 的载荷。
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

// UsageFailure 描述一次失败请求。
type UsageFailure struct {
	StatusCode int    `json:"StatusCode"`
	Body       string `json:"Body,omitempty"`
}
