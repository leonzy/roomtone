package app

import (
	"testing"
	"time"

	"songloft/internal/player"
)

// 新版客户端把能力放在嵌套 caps 里；服务端必须取 sample_rates 的最大值，
// 否则 RegisterPlayer 会把整段 caps 换成默认值，客户端声明的 codecs 丢失。
func TestLocalCapsFromNestedCaps(t *testing.T) {
	msg := playerWSMessage{
		Type: "register_player",
		Caps: &playerRegisterCaps{
			Codecs:      []string{"flac", "mp3", "aac", "wav"},
			SampleRates: []int{44100, 48000},
			MaxChannels: 2,
		},
	}
	caps := localCapsFromMessage(msg)
	if caps.MaxSampleRate != 48000 {
		t.Fatalf("MaxSampleRate = %d, want 48000", caps.MaxSampleRate)
	}
	if caps.Channels != 2 || caps.BitDepth != 16 {
		t.Fatalf("channels/bit depth = %d/%d, want 2/16", caps.Channels, caps.BitDepth)
	}
	if len(caps.Codecs) != 4 {
		t.Fatalf("codecs = %v, want 4", caps.Codecs)
	}
	if caps.Codecs[0] != player.CodecFLAC || caps.Codecs[3] != player.CodecWAV {
		t.Fatalf("codecs = %v, want flac..wav", caps.Codecs)
	}
	if !caps.SupportsVolume || !caps.SupportsSeek || caps.SyncTier != player.SyncExact {
		t.Fatalf("local sink caps 应支持音量/seek 且为 exact: %+v", caps)
	}
}

// 旧版客户端把 codecs / max_sample_rate 放在顶层，仍要能识别。
func TestLocalCapsFromLegacyTopLevel(t *testing.T) {
	msg := playerWSMessage{
		Type:    "register_player",
		Codecs:  []string{"flac"},
		MaxRate: 44100,
	}
	caps := localCapsFromMessage(msg)
	if caps.MaxSampleRate != 44100 || len(caps.Codecs) != 1 || caps.Codecs[0] != player.CodecFLAC {
		t.Fatalf("legacy caps 解析错误: %+v", caps)
	}
}

func TestPlayerOfflineTrackerCancel(t *testing.T) {
	tracker := newPlayerOfflineTracker()
	fired := make(chan struct{}, 1)
	tracker.schedule("p1", 20*time.Millisecond, func() { fired <- struct{}{} })
	tracker.cancel("p1")
	select {
	case <-fired:
		t.Fatal("cancel 后计时器不应触发")
	case <-time.After(60 * time.Millisecond):
	}
}

func TestPlayerOfflineTrackerFires(t *testing.T) {
	tracker := newPlayerOfflineTracker()
	fired := make(chan struct{}, 1)
	tracker.schedule("p1", 10*time.Millisecond, func() { fired <- struct{}{} })
	select {
	case <-fired:
	case <-time.After(time.Second):
		t.Fatal("计时器未触发")
	}
}
