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
