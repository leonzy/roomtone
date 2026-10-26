package player_test

import (
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
