package main

import (
	"strconv"
	"strings"
	"testing"
)

// TestStableIDGenReplicatesHost 校验插件推导 ID 的方式与宿主的 synthesizer.StableIDGenerator
// 完全一致：同样的输入得到同样的摘要，碰撞时的 -N 后缀按调用顺序追加。正因为如此，resolveKeys
// 算出的掩码 key 才能和插件在调度、用量请求里看到的真实 auth ID 对上，于是一把 key 不论可用还是
// 已禁用，都会出现在明细表里。
func TestStableIDGenReplicatesHost(t *testing.T) {
	g := newStableIDGen()
	kind := "openai-compatibility:modelscope"

	// 第一次调用：不带后缀的 12 位十六进制
	id1 := g.Next(kind, "ms-key-A", "https://api.modelscope.cn/v1", "")
	if len(id1) <= len(kind)+1 {
		t.Fatalf("id too short: %s", id1)
	}
	// 不同的 key 必须得出不同的 id
	id2 := g.Next(kind, "ms-key-B", "https://api.modelscope.cn/v1", "")
	if id1 == id2 {
		t.Fatalf("distinct keys produced same id: %s", id1)
	}

	// 制造一次碰撞：两条记录的 key、base、proxy 哈希出同一个 12 位 short（这里直接复用同样的
	// 输入）。宿主会按调用顺序追加 -1、-2 等后缀，插件必须这么做。
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

	// 可复现：新建的生成器必须得出完全相同的 id
	g2 := newStableIDGen()
	d1 := g2.Next(kind, "ms-key-A", "https://api.modelscope.cn/v1", "")
	if d1 != id1 {
		t.Fatalf("non-deterministic: %s != %s", d1, id1)
	}
}

// collisionSuffix 返回 12 位 short 之后的 -N 部分，没有时返回空串。
func collisionSuffix(id string) string {
	// id 形如 kind:short[-N]；short 是最后一个冒号后面的 12 个十六进制字符。
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

// TestMaskKeyShowsLast12NoEllipsis 校验 key 列显示 api-key 的末 12 位且不带省略号前缀，
// 这样运维可以把表里的掩码与 config.yaml 里某个 key 的尾部直接对上。
func TestMaskKeyShowsLast12NoEllipsis(t *testing.T) {
	full := "ms-b57ca7a1-2bbb-4867-a1ec-840ef1c90968"
	want := full[len(full)-12:]
	if got := maskKey(full); got != want {
		t.Fatalf("maskKey = %q, want last 12 %q", got, want)
	}
	if strings.Contains(maskKey(full), "\u2026") {
		t.Fatal("maskKey must not contain ellipsis")
	}
	// 长度不超过 12 的 key 原样显示
	if got := maskKey("short-key-1"); got != "short-key-1" {
		t.Fatalf("maskKey short = %q, want full", got)
	}
}

// TestBuildDedupMaps 校验跨 provider 重复出现的原始 key 会被去重，账号级 API 调用只算一次，
// 同时保留原始 key 到全部 auth ID 的分组，供禁用传播使用。
func TestBuildDedupMaps(t *testing.T) {
	raw := map[string]string{
		"openai-compatibility:modelscope:aaaabbbbcccc":  "ms-key-dup-1",
		"openai-compatibility:modelscope2:aaaabbbbcccc": "ms-key-dup-1",
		"openai-compatibility:modelscope:dddd11112222":  "ms-key-other-1",
	}
	siteBase := map[string]string{
		"openai-compatibility:modelscope:aaaabbbbcccc":  "https://modelscope.cn",
		"openai-compatibility:modelscope2:aaaabbbbcccc": "https://www.modelscope.ai",
		"openai-compatibility:modelscope:dddd11112222":  "https://modelscope.cn",
	}
	ordered := []string{
		"openai-compatibility:modelscope:aaaabbbbcccc",
		"openai-compatibility:modelscope2:aaaabbbbcccc",
		"openai-compatibility:modelscope:dddd11112222",
	}

	dedup, groups, _, maskToSite := buildDedupMaps(ordered, raw, siteBase)

	if len(dedup) != 2 {
		t.Fatalf("dedup count = %d, want 2 (duplicate key collapsed)", len(dedup))
	}
	if dedup[0] != ordered[0] || dedup[1] != ordered[2] {
		t.Fatalf("dedup order = %v, want first occurrence of each raw key", dedup)
	}

	siblings := groups["ms-key-dup-1"]
	if len(siblings) != 2 {
		t.Fatalf("sibling count = %d, want 2", len(siblings))
	}
	if siblings[0] != ordered[0] || siblings[1] != ordered[1] {
		t.Fatalf("sibling order = %v, want config order", siblings)
	}

	if got := maskToSite[maskKey("ms-key-dup-1")]; got != "https://modelscope.cn" {
		t.Fatalf("maskToSite = %q, want the first provider's site", got)
	}
}

// TestStableIDGenSameKeyDifferentProviders 校验同一把原始 key 不会跨 provider 合并：由于
// provider 名参与哈希，每家都会得到不同的 auth ID。
func TestStableIDGenSameKeyDifferentProviders(t *testing.T) {
	g := newStableIDGen()
	key := "ms-key-same-account"

	idA := g.Next("openai-compatibility:modelscope", key, "https://modelscope.cn", "")
	idB := g.Next("openai-compatibility:modelscope2", key, "https://modelscope.cn", "")

	if idA == idB {
		t.Fatalf("same key across providers must produce different auth IDs: %s", idA)
	}

	// 同一家、同一 key、同一 base 和 proxy 必然碰撞，并拿到 -1 后缀。
	idC := g.Next("openai-compatibility:modelscope", key, "https://modelscope.cn", "")
	if collisionSuffix(idC) != "-1" {
		t.Fatalf("same-input collision suffix = %q, want %q", collisionSuffix(idC), "-1")
	}
}
