package ratelimit

import (
	"encoding/json"
	"testing"
)

func TestNormalizeAPIResponseCamelCase(t *testing.T) {
	raw := []byte(`{"Code":200,"Data":{"Name":"liuzhijie","Email":"test@qq.com"},"Message":"success","Success":true,"RequestId":"abc"}`)
	resp, err := NormalizeAPIResponse(raw)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !resp.Success {
		t.Fatal("expected success=true")
	}
	if resp.Code != 200 {
		t.Fatalf("code = %d, want 200", resp.Code)
	}
	var li LoginInfo
	if err := json.Unmarshal(resp.Data, &li); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if li.Name != "liuzhijie" || li.Email != "test@qq.com" {
		t.Fatalf("unexpected: %+v", li)
	}
}

func TestNormalizeAPIResponseSnakeCase(t *testing.T) {
	raw := []byte(`{"success":true,"request_id":"xyz","data":{"total_balance":204,"available_balance":204,"frozen_amount":0}}`)
	resp, err := NormalizeAPIResponse(raw)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !resp.Success {
		t.Fatal("expected success=true")
	}
	var b MagicCubeBalance
	if err := json.Unmarshal(resp.Data, &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if b.TotalBalance != 204 || b.AvailableBalance != 204 || b.FrozenAmount != 0 {
		t.Fatalf("unexpected: %+v", b)
	}
}

func TestNormalizeAPIResponseError(t *testing.T) {
	raw := []byte(`{"Code":10010101003,"Message":"access forbidden","Success":false}`)
	resp, err := NormalizeAPIResponse(raw)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if resp.Success {
		t.Fatal("expected success=false for error code")
	}
	if resp.Code != 10010101003 {
		t.Fatalf("code = %d, want 10010101003", resp.Code)
	}
}

func TestNormalizeAPIResponseUnparseable(t *testing.T) {
	raw := []byte(`404 page not found`)
	_, err := NormalizeAPIResponse(raw)
	if err == nil {
		t.Fatal("expected error for unparseable response")
	}
}

// TestNormalizeAPIResponseMixedCase 覆盖混合信封：顶层 "Code" 是大驼峰而 "data" 是小写 ——
// GET /api/v1/inference/rate-limit 返回的就是这种格式。这正是最初那个 bug：大驼峰解析器匹配到
// "Code":200 后就带着空 Data 提前返回，导致 currentUsagePerDay 永远解码成 0。
func TestNormalizeAPIResponseMixedCase(t *testing.T) {
	raw := []byte(`{"Code":200,"Data":{"currentUsagePerDay":6},"Message":"success","RequestId":"abc","Success":true}`)
	resp, err := NormalizeAPIResponse(raw)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !resp.Success {
		t.Fatal("expected success=true")
	}
	if resp.Code != 200 {
		t.Fatalf("code = %d, want 200", resp.Code)
	}
	var rl InferenceRateLimit
	if err := json.Unmarshal(resp.Data, &rl); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rl.CurrentUsagePerDay != 6 {
		t.Fatalf("currentUsagePerDay = %d, want 6", rl.CurrentUsagePerDay)
	}
}

// TestNormalizeAPIResponseLowerData 覆盖另一种混合信封："Code" 是大驼峰而 "data" 全小写 —— 在
// 某些 /api/v1/* 端点上也会看到。Go 的大小写不敏感 json.Unmarshal 应当能用带 "Data" tag 的那一
// 次结构体解析把它取到。
func TestNormalizeAPIResponseLowerData(t *testing.T) {
	raw := []byte(`{"Code":200,"data":{"currentUsagePerDay":42},"Message":"success","Success":true}`)
	resp, err := NormalizeAPIResponse(raw)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !resp.Success {
		t.Fatal("expected success=true")
	}
	if resp.Code != 200 {
		t.Fatalf("code = %d, want 200", resp.Code)
	}
	var rl InferenceRateLimit
	if err := json.Unmarshal(resp.Data, &rl); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rl.CurrentUsagePerDay != 42 {
		t.Fatalf("currentUsagePerDay = %d, want 42", rl.CurrentUsagePerDay)
	}
}

func TestTransactionRecordFields(t *testing.T) {
	// 校验 json tag 与真实 API 响应一致
	raw := []byte(`{"gmt_created":"2026-08-13","type":"EARN","rule_key":"daily_active","category_key":"daily","title":"注册并登录","description":"注册账号后每日登录即可获取","category":"每日发放","total_amount":200,"count":1,"expire_days":1,"expire_at":"2026-08-14 08:28:41","days_remaining":1}`)
	var rec TransactionRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec.Type != "EARN" || rec.Title != "注册并登录" || rec.TotalAmount != 200 {
		t.Fatalf("unexpected: %+v", rec)
	}
}

func TestSPENDConfirmRecordFields(t *testing.T) {
	raw := []byte(`{"gmt_created":"2026-08-13","type":"SPEND_CONFIRM","rule_key":"api_inference","category_key":"api_inference","model_tier":"ultra","title":"","description":"","category":"","total_amount":94,"count":47}`)
	var rec TransactionRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rec.ModelTier != "ultra" || rec.Title != "" || rec.TotalAmount != 94 {
		t.Fatalf("unexpected: %+v", rec)
	}
}
