package player_test

import (
	"context"
	"encoding/json"
	"testing"

	"songloft/internal/player"
	"songloft/internal/player/sink"
)

// TestSyncTierJSONIsString 锁住 /players 与 /groups 的 sync_tier 形态一致。
//
// 回归背景：Caps.SyncTier 以前没有 MarshalJSON，/players 输出 "sync_tier": 0，
// 而 /groups 输出 "exact"；客户端按字符串解析，拿到数字直接抛类型错误。
func TestSyncTierJSONIsString(t *testing.T) {
	for _, tt := range []struct {
		tier sink.SyncTier
		want string
	}{
		{sink.SyncExact, `"exact"`},
		{sink.SyncAligned, `"aligned"`},
	} {
		got, err := json.Marshal(tt.tier)
		if err != nil {
			t.Fatalf("Marshal(%v): %v", tt.tier, err)
		}
		if string(got) != tt.want {
			t.Fatalf("Marshal(%v) = %s, want %s", tt.tier, got, tt.want)
		}
	}
}

// TestCapsSyncTierRoundTrip 兼容旧库：历史 players.capabilities 里是数字 0/1。
func TestCapsSyncTierRoundTrip(t *testing.T) {
	c := player.Caps{SyncTier: sink.SyncAligned, MaxSampleRate: 48000}
	raw := player.CapsJSON(c)
	if got := player.ParseCaps(raw).SyncTier; got != sink.SyncAligned {
		t.Fatalf("round trip SyncTier = %v, want aligned（raw=%s）", got, raw)
	}

	legacy := `{"codecs":["l16"],"max_sample_rate":44100,"sync_tier":1}`
	if got := player.ParseCaps(legacy).SyncTier; got != sink.SyncAligned {
		t.Fatalf("legacy int sync_tier = %v, want aligned", got)
	}
}

// TestSyncPreciseJSON B1：precise 档——MarshalJSON 输出 "precise"；
// UnmarshalJSON 接受 "precise" 与数字 2（双兼容），历史值不变；
// 拒绝未知字符串。
func TestSyncPreciseJSON(t *testing.T) {
	got, err := json.Marshal(sink.SyncPrecise)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `"precise"` {
		t.Fatalf("Marshal(SyncPrecise) = %s, want \"precise\"", got)
	}
	if sink.SyncPrecise.String() != "precise" {
		t.Fatalf("String = %q, want precise", sink.SyncPrecise.String())
	}
	// 数字语义锁死：exact=0 / aligned=1 / precise=2（历史落库行不能重排）
	if sink.SyncExact != 0 || sink.SyncAligned != 1 || sink.SyncPrecise != 2 {
		t.Fatalf("SyncTier 数字语义被改动: exact=%d aligned=%d precise=%d",
			sink.SyncExact, sink.SyncAligned, sink.SyncPrecise)
	}
	// UnmarshalJSON 双兼容
	for _, tt := range []struct {
		in   string
		want sink.SyncTier
	}{
		{`"precise"`, sink.SyncPrecise},
		{`2`, sink.SyncPrecise},
		{`"exact"`, sink.SyncExact},
		{`0`, sink.SyncExact},
		{`"aligned"`, sink.SyncAligned},
		{`1`, sink.SyncAligned},
		{`""`, sink.SyncExact},
	} {
		var got sink.SyncTier
		if err := json.Unmarshal([]byte(tt.in), &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", tt.in, err)
		}
		if got != tt.want {
			t.Fatalf("Unmarshal(%s) = %v, want %v", tt.in, got, tt.want)
		}
	}
	// 拒绝未知字符串
	var tier sink.SyncTier
	if err := json.Unmarshal([]byte(`"bogus"`), &tier); err == nil {
		t.Fatal(`Unmarshal("bogus") 应当报错`)
	}
	// Caps 落库往返
	raw := player.CapsJSON(player.Caps{SyncTier: sink.SyncPrecise})
	if got := player.ParseCaps(raw).SyncTier; got != sink.SyncPrecise {
		t.Fatalf("round trip SyncTier = %v, want precise（raw=%s）", got, raw)
	}
}

// TestGroupTierPrecise B1：组档位取**最差**成员（rank 最大）——
// precise+exact → exact；exact+aligned → aligned；precise+aligned → aligned。
func TestGroupTierPrecise(t *testing.T) {
	m, _, _ := newTestManager(t, "a", "b", "c")
	ctx := context.Background()
	gid := m.GroupIDOf("a")
	if _, err := m.AddMember(ctx, gid, "b"); err != nil {
		t.Fatalf("addmember b: %v", err)
	}
	setTier := func(id string, tier sink.SyncTier) {
		p, err := m.GetPlayer(id)
		if err != nil {
			t.Fatal(err)
		}
		p.Caps.SyncTier = tier
		// 直接写回 manager 内部（测试捷径：经 RegisterPlayer 保留原值路径）
		if err := m.RegisterPlayer(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		a, b sink.SyncTier
		want sink.SyncTier
	}{
		{sink.SyncPrecise, sink.SyncExact, sink.SyncExact},
		{sink.SyncExact, sink.SyncAligned, sink.SyncAligned},
		{sink.SyncPrecise, sink.SyncAligned, sink.SyncAligned},
		{sink.SyncPrecise, sink.SyncPrecise, sink.SyncPrecise},
	}
	for _, c := range cases {
		setTier("a", c.a)
		setTier("b", c.b)
		g, err := m.GetGroup(gid)
		if err != nil {
			t.Fatal(err)
		}
		if g.SyncTier != c.want.String() {
			t.Fatalf("组档位(%v+%v) = %v, want %v", c.a, c.b, g.SyncTier, c.want)
		}
	}
}
