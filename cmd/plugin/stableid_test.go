package main

import (
	"strconv"
	"strings"
	"testing"
)

// TestStableIDGenReplicatesHost verifies the plugin's ID derivation matches the
// host's synthesizer.StableIDGenerator: same digest for the same inputs, and
// the "-N" collision suffix is applied in call order. This is what makes the
// masked keys (from resolveKeys) line up with the real auth IDs the plugin
// observes in scheduler/usage requests, so every key — available or disabled —
// appears in the key-detail table.
func TestStableIDGenReplicatesHost(t *testing.T) {
	g := newStableIDGen()
	kind := "openai-compatibility:modelscope"

	// First call: unsuffixed 12-hex.
	id1 := g.Next(kind, "ms-key-A", "https://api.modelscope.cn/v1", "")
	if len(id1) <= len(kind)+1 {
		t.Fatalf("id too short: %s", id1)
	}
	// A distinct key must produce a distinct id.
	id2 := g.Next(kind, "ms-key-B", "https://api.modelscope.cn/v1", "")
	if id1 == id2 {
		t.Fatalf("distinct keys produced same id: %s", id1)
	}

	// Reproduce a collision: two entries whose (key, base, proxy) hash to the
	// same 12-hex short (here by reusing the exact same inputs). The host
	// appends "-1", "-2", ... in call order; the plugin must do the same.
	dup := []string{
		g.Next(kind, "ms-dup", "https://api.modelscope.cn/v1", ""),
		g.Next(kind, "ms-dup", "https://api.modelscope.cn/v1", ""),
		g.Next(kind, "ms-dup", "https://api.modelscope.cn/v1", ""),
	}
	for i, c := range dup {
		want := ""
		if i > 0 {
			want = "-" + strconv.Itoa(i)
		}
		if got := collisionSuffix(c); got != want {
			t.Fatalf("collision call #%d suffix = %q, want %q (id %s)", i, got, want, c)
		}
	}

	// Deterministic: a fresh generator must reproduce the exact same ids.
	g2 := newStableIDGen()
	d1 := g2.Next(kind, "ms-key-A", "https://api.modelscope.cn/v1", "")
	if d1 != id1 {
		t.Fatalf("non-deterministic: %s != %s", d1, id1)
	}
}

// collisionSuffix returns the "-N" part after the 12-hex short, or "" if none.
func collisionSuffix(id string) string {
	// id = kind:short[-N]; the short is 12 hex chars right after the last ':'.
	idx := strings.LastIndex(id, ":")
	if idx < 0 {
		return ""
	}
	tail := id[idx+1:]
	if i := strings.Index(tail, "-"); i >= 0 {
		return tail[i:]
	}
	return ""
}

// TestMaskKeyShowsLast12NoEllipsis verifies the key column shows the last 12
// chars of an api-key with no ellipsis prefix, so operators can match a masked
// entry against the tail of a key in config.yaml.
func TestMaskKeyShowsLast12NoEllipsis(t *testing.T) {
	full := "ms-b57ca7a1-2bbb-4867-a1ec-840ef1c90968"
	want := full[len(full)-12:]
	if got := maskKey(full); got != want {
		t.Fatalf("maskKey = %q, want last 12 %q", got, want)
	}
	if strings.Contains(maskKey(full), "\u2026") {
		t.Fatal("maskKey must not contain ellipsis")
	}
	// Short keys (<=12) are shown in full.
	if got := maskKey("short-key-1"); got != "short-key-1" {
		t.Fatalf("maskKey short = %q, want full", got)
	}
}
