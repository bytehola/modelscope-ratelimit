package ratelimit

import (
	"testing"
)

// TestSkipProviderOn429Parsed 验证 skip_provider_on_429 能从配置 map 中正确解析出来。
func TestSkipProviderOn429Parsed(t *testing.T) {
	t.Run("true", func(t *testing.T) {
		c, err := ConfigFromMap(map[string]any{
			"providers":            []any{"modelscope"},
			"skip_provider_on_429": true,
		})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if !c.SkipProviderOn429 {
			t.Fatal("SkipProviderOn429 should be true")
		}
	})
	t.Run("false_default", func(t *testing.T) {
		c, err := ConfigFromMap(map[string]any{
			"providers": []any{"modelscope"},
		})
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if c.SkipProviderOn429 {
			t.Fatal("SkipProviderOn429 should default to false")
		}
	})
}

func TestUseResponsesParsingAndMatching(t *testing.T) {
	tests := []struct {
		name  string
		raw   any
		model string
		want  bool
	}{
		{"default_disabled", nil, "glm-5.2", false},
		{"empty_array", []any{}, "glm-5.2", false},
		{"wildcard_string", "*", "any-model", true},
		{"specific_string", "glm-5.2", "glm-5.2", true},
		{"specific_string_other", "glm-5.2", "qwen-code", false},
		{"array", []any{"glm-5.2", "qwen-code"}, "qwen-code", true},
		{"array_other", []any{"glm-5.2", "qwen-code"}, "other", false},
		{"legacy_true", true, "any-model", true},
		{"legacy_false", false, "any-model", false},
		// 管理面板把 use_responses 渲染成文本框，所以运维写的列表或关键字
		// 永远以一个标量字符串送达。
		{"string_true", "true", "any-model", true},
		{"string_true_upper", "TRUE", "any-model", true},
		{"string_false", "false", "any-model", false},
		{"string_false_upper", "FALSE", "any-model", false},
		{"string_unquoted_bracket_list", `["Qwen/Qwen3.8-Flash-Next", "deepseek-ai/DeepSeek-V4-Pro-0813"]`, "deepseek-ai/DeepSeek-V4-Pro-0813", true},
		{"string_unquoted_bracket_list_other", `["Qwen/Qwen3.8-Flash-Next", "deepseek-ai/DeepSeek-V4-Pro-0813"]`, "ZhipuAI/GLM-5.2", false},
		{"string_single_quoted_items", "['ZhipuAI/GLM-5.2', 'Qwen/Qwen2.5-72B-Instruct']", "Qwen/Qwen2.5-72B-Instruct", true},
		{"string_bracket_list_no_quotes", "[ZhipuAI/GLM-5.2,Qwen/Qwen2.5-72B-Instruct]", "ZhipuAI/GLM-5.2", true},
		{"string_comma_separated", "ZhipuAI/GLM-5.2, Qwen/Qwen2.5-72B-Instruct", "Qwen/Qwen2.5-72B-Instruct", true},
		{"string_comma_separated_other", "ZhipuAI/GLM-5.2, Qwen/Qwen2.5-72B-Instruct", "deepseek-ai/DeepSeek-V4-Pro-0813", false},
		{"string_bracket_wildcard", `["*"]`, "any-model", true},
		{"string_empty_brackets", "[]", "any-model", false},
		{"string_garbage", "not-a-model-name", "glm-5.2", false},
		{"array_with_bracket_string", []any{`["a-model", "b-model"]`}, "b-model", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := map[string]any{}
			if tt.raw != nil {
				m["use_responses"] = tt.raw
			}
			c, err := ConfigFromMap(m)
			if err != nil {
				t.Fatalf("ConfigFromMap: %v", err)
			}
			if got := c.UsesResponses(tt.model); got != tt.want {
				t.Fatalf("UsesResponses(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

// TestUseResponsesParsedShape 固定住各种可接受写法展开后的结果：面板里输入的方括号字符串必须得
// 到与真正 YAML 序列相同的模型列表，并且重复项在启动横幅里被折叠掉。
func TestUseResponsesParsedShape(t *testing.T) {
	tests := []struct {
		name string
		raw  any
		want []string
	}{
		{"wildcard", "*", []string{"*"}},
		{"keyword_true", "true", []string{"*"}},
		{"keyword_false", "false", nil},
		{"legacy_bool", true, []string{"*"}},
		{"single", "glm-5.2", []string{"glm-5.2"}},
		{"bracket_list", `["Qwen/Qwen3.8-Flash-Next", "deepseek-ai/DeepSeek-V4-Pro-0813"]`, []string{"Qwen/Qwen3.8-Flash-Next", "deepseek-ai/DeepSeek-V4-Pro-0813"}},
		{"comma_list", "Qwen/Qwen3.8-Flash-Next, deepseek-ai/DeepSeek-V4-Pro-0813", []string{"Qwen/Qwen3.8-Flash-Next", "deepseek-ai/DeepSeek-V4-Pro-0813"}},
		{"yaml_sequence", []any{"Qwen/Qwen3.8-Flash-Next", "deepseek-ai/DeepSeek-V4-Pro-0813"}, []string{"Qwen/Qwen3.8-Flash-Next", "deepseek-ai/DeepSeek-V4-Pro-0813"}},
		{"dedup_case_insensitive", "glm-5.2, qwen-code, GLM-5.2 ,glm-5.2", []string{"glm-5.2", "qwen-code"}},
		{"empty", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := ConfigFromMap(map[string]any{"use_responses": tt.raw})
			if err != nil {
				t.Fatalf("ConfigFromMap: %v", err)
			}
			got := c.UseResponses
			if len(got) != len(tt.want) {
				t.Fatalf("UseResponses = %q, want %q", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("UseResponses = %q, want %q", got, tt.want)
				}
			}
		})
	}
}

// TestToFloat64EdgeCases 验证 toFloat64 能处理常见的 YAML/JSON 数值写法。
func TestToFloat64EdgeCases(t *testing.T) {
	tests := []struct {
		name  string
		input any
		want  float64
		ok    bool
	}{
		{"int", 5, 5.0, true},
		{"int64", int64(10), 10.0, true},
		{"float64", 0.25, 0.25, true},
		{"string_int", "60", 60.0, true},
		{"string_float", "0.5", 0.5, true},
		{"string_invalid", "abc", 0, false},
		{"nil", nil, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := toFloat64(tt.input)
			if tt.ok {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tt.want {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			} else {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
			}
		})
	}
}

// TestConfigNoManagedModelsField 验证配置 map 里出现 managed_models 时会被静默忽略（不报错，也
// 不影响配置）。
func TestConfigNoManagedModelsField(t *testing.T) {
	c, err := ConfigFromMap(map[string]any{
		"providers":      []any{"modelscope"},
		"managed_models": []any{"Qwen-72B"}, // 应当被静默忽略
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 已经没有 ManagedModels 字段可校验，这里只确认不会崩溃。
	_ = c
}
