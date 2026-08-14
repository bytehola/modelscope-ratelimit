package ratelimit

import (
	"strings"
	"testing"
)

func TestRenderGuidanceHTML(t *testing.T) {
	cfg := DefaultConfig()
	html := RenderGuidanceHTML(cfg, GuidanceMissing, "")
	for _, want := range []string{
		"Modelscope 限流",
		"配置引导",
		"providers",
		"management_key",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("guidance HTML missing %q", want)
		}
	}
}
