package ratelimit

import "testing"

func TestManagesProviderPrefix(t *testing.T) {
	c := cfgWith("modelscope")
	cases := map[string]bool{
		"modelscope":                      true, // bare configured name (exact)
		"openai-compatible-modelscope":    true, // host runtime provider key (prefixed)
		"MODELSCOPE":                      true, // case-insensitive
		"openai-compatibility":            false,
		"openai-compatibility:modelscope": true,  // colon auth-kind prefix form
		"modelscope2":                     false, // similar name, no token match
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
	// "modelscope" and "modelscope-my" must not collide: the old token
	// matching split "modelscope-my" on "-" and matched the "modelscope"
	// token. The new prefix-strip + exact match must distinguish them.
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
	// ManagesProvider must agree with ProviderIndex.
	for provider, idx := range cases {
		want := idx >= 0
		if got := c.ManagesProvider(provider); got != want {
			t.Errorf("ManagesProvider(%q) = %v, want %v", provider, got, want)
		}
	}
}
