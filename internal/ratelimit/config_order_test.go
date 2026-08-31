package ratelimit

import (
	"testing"
)

// TestSkipProviderOn429Parsed verifies that skip_provider_on_429 is correctly
// parsed from the config map (now positioned after providers).
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

// TestToFloat64EdgeCases verifies the toFloat64 helper handles all common
// YAML/JSON numeric representations.
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

// TestConfigNoManagedModelsField verifies that managed_models in the raw map
// is silently ignored (no error, no effect on config).
func TestConfigNoManagedModelsField(t *testing.T) {
	c, err := ConfigFromMap(map[string]any{
		"providers":      []any{"modelscope"},
		"managed_models": []any{"Qwen-72B"}, // should be silently ignored
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No ManagedModels field to check, just verify it doesn't crash.
	_ = c
}
