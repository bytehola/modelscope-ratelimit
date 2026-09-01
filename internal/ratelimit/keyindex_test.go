package ratelimit

import "testing"

func TestManagesProviderPrefix(t *testing.T) {
	c := cfgWith("modelscope")
	cases := map[string]bool{
		"modelscope":                      true, // 裸配置名（精确匹配）
		"openai-compatible-modelscope":    true, // 宿主运行时 provider key（带前缀）
		"MODELSCOPE":                      true, // 忽略大小写
		"openai-compatibility":            false,
		"openai-compatibility:modelscope": true,  // 冒号形式的凭据类型前缀
		"modelscope2":                     false, // 名字相近，但不做 token 匹配
		"other":                           false,
		"":                                false,
	}
	for provider, want := range cases {
		if got := c.ManagesProvider(provider); got != want {
			t.Errorf("ManagesProvider(%q) = %v, want %v", provider, got, want)
		}
	}
	if DefaultConfig().ManagesProvider("modelscope") {
		t.Error("empty Providers must not manage anything")
	}
}

func TestManagesProviderDisambiguation(t *testing.T) {
	// "modelscope" 和 "modelscope-my" 不能互相命中：旧的 token 匹配会把 "modelscope-my" 按
	// "-" 拆开并匹配到 "modelscope" 这个 token。新的“剥前缀 + 精确匹配”必须能区分二者。
	c := cfgWith("modelscope", "modelscope-my")
	cases := map[string]int{
		"modelscope":                             0,
		"openai-compatible-modelscope":           0,
		"openai-compatibility:modelscope:abc":    0,
		"modelscope-my":                          1,
		"openai-compatible-modelscope-my":        1,
		"openai-compatibility:modelscope-my:abc": 1,
		"modelscope2":                            -1,
		"my":                                     -1,
		"openai-compatibility":                   -1,
		"":                                       -1,
	}
	for provider, want := range cases {
		got := c.ProviderIndex(provider)
		if got != want {
			t.Errorf("ProviderIndex(%q) = %d, want %d", provider, got, want)
		}
	}
	// ManagesProvider 的结论必须与 ProviderIndex 一致。
	for provider, idx := range cases {
		want := idx >= 0
		if got := c.ManagesProvider(provider); got != want {
			t.Errorf("ManagesProvider(%q) = %v, want %v", provider, got, want)
		}
	}
}
