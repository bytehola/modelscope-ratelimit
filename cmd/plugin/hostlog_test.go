package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	modelscope "modelscope-ratelimit/internal/ratelimit"
)

// withHostLog 挂上一个假的 host.log 回调和非空的宿主句柄，并返回插件本应交给 CLIProxyAPI 的日志行。
func withHostLog(t *testing.T, fail bool) *[]rpcHostLogRequest {
	t.Helper()
	var sent []rpcHostLogRequest
	origCall := hostLogCall
	origReady := hostReady
	origDisabled := hostLogDisabled.Load()
	hostReady = func() bool { return true }
	hostLogDisabled.Store(false)
	hostLogCall = func(method string, payload []byte) ([]byte, error) {
		if method != pluginabi.MethodHostLog {
			return nil, errors.New("unexpected host method " + method)
		}
		if fail {
			return nil, errors.New("host.log unavailable")
		}
		var req rpcHostLogRequest
		if err := json.Unmarshal(payload, &req); err != nil {
			return nil, err
		}
		sent = append(sent, req)
		return []byte(`{}`), nil
	}
	t.Cleanup(func() {
		hostLogCall = origCall
		hostReady = origReady
		hostLogDisabled.Store(origDisabled)
	})
	return &sent
}

func captureStdLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	origOut := log.Writer()
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(origOut) })
	return buf
}

func TestLogInfoRoutesToHostLog(t *testing.T) {
	sent := withHostLog(t, false)
	buf := captureStdLog(t)

	logInfo("responses served provider=%s model=%s", "modelscope-my", "glm-5.2")

	if len(*sent) != 1 {
		t.Fatalf("host.log calls = %d, want 1", len(*sent))
	}
	got := (*sent)[0]
	if got.Level != "info" {
		t.Fatalf("level = %q, want info", got.Level)
	}
	if !strings.HasPrefix(got.Message, logPrefix) {
		t.Fatalf("message lost the plugin prefix: %q", got.Message)
	}
	if got.Message != logPrefix+"responses served provider=modelscope-my model=glm-5.2" {
		t.Fatalf("message = %q", got.Message)
	}
	if buf.Len() != 0 {
		t.Fatalf("plugin lines must not also go to stderr when host.log works: %q", buf.String())
	}
}

func TestLogDebugUsesDebugLevelAndRequestCallbackID(t *testing.T) {
	sent := withHostLog(t, false)

	logDebugRequest("cb-77", "responses attempt provider=%s", "modelscope-my")

	if len(*sent) != 1 {
		t.Fatalf("host.log calls = %d, want 1", len(*sent))
	}
	got := (*sent)[0]
	if got.Level != "debug" {
		t.Fatalf("level = %q, want debug", got.Level)
	}
	if got.HostCallbackID != "cb-77" {
		t.Fatalf("host_callback_id = %q, want cb-77 so the host tags the request id", got.HostCallbackID)
	}
}

func TestStoreLoggerReachesHostLog(t *testing.T) {
	sent := withHostLog(t, false)

	// 核心状态机通过注入的 sink 打日志。禁用事件是运维最先找的东西，所以它也必须落到宿主日志里。
	// 用唯一的 auth id，避免污染其他用例共用的 store 状态。
	store.DisableGlobal("auth-logprobe", "glm-5.2", store.Now())

	found := false
	for _, req := range *sent {
		if strings.Contains(req.Message, "auth-logprobe") && strings.Contains(req.Message, "globally disabled") {
			found = true
		}
	}
	if !found {
		t.Fatalf("core disable event never reached host.log: %+v", *sent)
	}
}

func TestLogFallsBackToStderrWhenHostLogFails(t *testing.T) {
	sent := withHostLog(t, true)
	buf := captureStdLog(t)

	logInfo("first line %d", 1)
	logInfo("second line %d", 2)

	if len(*sent) != 0 {
		t.Fatalf("failing host.log should record nothing, got %+v", *sent)
	}
	out := buf.String()
	if !strings.Contains(out, "host.log unavailable") {
		t.Fatalf("fallback notice missing from stderr: %q", out)
	}
	if !strings.Contains(out, "first line 1") || !strings.Contains(out, "second line 2") {
		t.Fatalf("plugin lines lost after host.log failure: %q", out)
	}
}

func TestLogFallsBackBeforeHostInit(t *testing.T) {
	calls := 0
	origCall := hostLogCall
	origReady := hostReady
	origDisabled := hostLogDisabled.Load()
	t.Cleanup(func() {
		hostLogCall = origCall
		hostReady = origReady
		hostLogDisabled.Store(origDisabled)
	})
	hostReady = func() bool { return false }
	hostLogDisabled.Store(false)
	hostLogCall = func(string, []byte) ([]byte, error) { calls++; return []byte(`{}`), nil }

	buf := captureStdLog(t)
	logInfo("early startup line")

	if calls != 0 {
		t.Fatalf("host.log must not be called before the host handle exists, calls=%d", calls)
	}
	if !strings.Contains(buf.String(), "early startup line") {
		t.Fatalf("expected stderr fallback, got %q", buf.String())
	}
}

func TestLogBodyExcerptIsSingleLineAndBounded(t *testing.T) {
	long := strings.Repeat("a", maxLogBody+50) + "\nsecond line"
	got := logBodyExcerpt(long)
	if strings.Contains(got, "\n") {
		t.Fatalf("excerpt must stay on one line: %q", got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("truncated excerpt should be marked, got %q", got)
	}
	if len(got) > maxLogBody+20 {
		t.Fatalf("excerpt not bounded: %d chars", len(got))
	}
}

// countBanner 统计捕获到的 host.log 里配置横幅有几行。
func countBanner(sent *[]rpcHostLogRequest) int {
	n := 0
	for _, req := range *sent {
		if strings.Contains(req.Message, "configured v") {
			n++
		}
	}
	return n
}

// TestConfigBannerIsDeferredOncePerChange 同时钉住横幅的两条规则：宿主在挂接日志文件之前就会加载
// 插件，因此横幅必须推迟到有运行期调用证明日志出口已存在之后再写；同一份配置反复
// register/reconfigure 只能产生一行。
func TestConfigBannerIsDeferredOncePerChange(t *testing.T) {
	sent := withHostLog(t, false)
	origLast, origPending := lastConfigBanner, pendingConfigBanner
	t.Cleanup(func() {
		lastConfigBanner, pendingConfigBanner = origLast, origPending
	})
	lastConfigBanner, pendingConfigBanner = "", ""

	cfg := &modelscope.Config{
		Providers:                 []string{"modelscope-ai-deepseek2", "modelscope-my"},
		InsufficientQuotaCooldown: 0.1,
		ManagementKey:             "super-secret-mgmt-key",
	}
	logConfigBanner(cfg)
	logConfigBanner(cfg)
	logConfigBanner(cfg)
	if n := countBanner(sent); n != 0 {
		t.Fatalf("banner must stay pending until the host log sink is attached, got %d", n)
	}

	flushConfigBanner()
	if n := countBanner(sent); n != 1 {
		t.Fatalf("banner lines after flush = %d, want 1", n)
	}
	flushConfigBanner()
	if n := countBanner(sent); n != 1 {
		t.Fatalf("flush must be idempotent, got %d lines", n)
	}
	logConfigBanner(cfg)
	flushConfigBanner()
	if n := countBanner(sent); n != 1 {
		t.Fatalf("unchanged config must not repeat the banner, got %d lines", n)
	}

	changed := &modelscope.Config{Providers: []string{"modelscope-my"}, SkipProviderOn429: true}
	logConfigBanner(changed)
	flushConfigBanner()
	if n := countBanner(sent); n != 2 {
		t.Fatalf("a real change must print a new banner, got %d lines", n)
	}
	last := (*sent)[len(*sent)-1].Message
	if !strings.Contains(last, "skip_provider_on_429=true") {
		t.Fatalf("changed settings not reflected in banner: %q", last)
	}
	if strings.Contains(last, "super-secret-mgmt-key") {
		t.Fatal("management key must never reach the log")
	}
}
