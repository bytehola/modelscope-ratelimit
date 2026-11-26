package main

import "testing"

func TestMaskToken(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  string
	}{
		{name: "empty", token: "", want: "***"},
		{name: "short", token: "short", want: "***"},
		{name: "long", token: "demo-token-value", want: "demo-t...alue"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := maskToken(test.token); got != test.want {
				t.Fatalf("maskToken(%q) = %q, want %q", test.token, got, test.want)
			}
		})
	}
}
