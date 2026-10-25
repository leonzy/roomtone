package player

import (
	"encoding/binary"
	"math"
	"testing"
)

func TestReplayGainGainDBModes(t *testing.T) {
	rg := ReplayGain{TrackGain: -6, TrackPeak: 0.5, AlbumGain: -3, AlbumPeak: 0.5}

	if got := rg.GainDB(ReplayGainOff, false); got != 0 {
		t.Fatalf("off 模式应返回 0，实际 %v", got)
	}
	if got := rg.GainDB(ReplayGainTrack, false); got != -6 {
		t.Fatalf("track 模式应为 -6dB，实际 %v", got)
	}
	if got := rg.GainDB(ReplayGainAlbum, false); got != -3 {
		t.Fatalf("album 模式应为 -3dB，实际 %v", got)
	}

	// album 缺增益时退回 track，避免整张专辑突然变响。
	only := ReplayGain{TrackGain: -4, TrackPeak: 0.5}
	if got := only.GainDB(ReplayGainAlbum, false); got != -4 {
		t.Fatalf("album 缺增益应退回 track(-4)，实际 %v", got)
	}

	// 没有任何标签：不加增益。
	if got := (ReplayGain{}).GainDB(ReplayGainTrack, false); got != 0 {
		t.Fatalf("无标签应返回 0，实际 %v", got)
	}
	// 非法模式当作关闭。
	if got := rg.GainDB(ReplayGainMode("bogus"), false); got != 0 {
		t.Fatalf("非法模式应返回 0，实际 %v", got)
	}
}

func TestReplayGainClippingPrevention(t *testing.T) {
	// 峰值 0.5 → 最多 +6.02dB；请求 +9dB 应被压到 6.02dB。
	rg := ReplayGain{TrackGain: 9, TrackPeak: 0.5}
	got := rg.GainDB(ReplayGainTrack, true)
	if math.Abs(got-6.0206) > 0.01 {
		t.Fatalf("防削波应压到 6.02dB，实际 %.4f", got)
	}
	if got := rg.GainDB(ReplayGainTrack, false); got != 9 {
		t.Fatalf("关闭防削波应保留 9dB，实际 %v", got)
	}

	// 负增益不受峰值限制（压音量永远不会削波）。
	neg := ReplayGain{TrackGain: -8, TrackPeak: 1.0}
	if got := neg.GainDB(ReplayGainTrack, true); got != -8 {
		t.Fatalf("负增益不应被峰值抬高，实际 %v", got)
	}

	// 没有峰值标签时不做限制。
	noPeak := ReplayGain{TrackGain: 9}
	if got := noPeak.GainDB(ReplayGainTrack, true); got != 9 {
		t.Fatalf("缺峰值标签应保留 9dB，实际 %v", got)
	}
}

func TestGainFactor(t *testing.T) {
	cases := []struct {
		db   float64
		want float64
	}{
		{0, 1},
		{6.0206, 2},
		{-6.0206, 0.5},
	}
	for _, c := range cases {
		if got := GainFactor(c.db); math.Abs(got-c.want) > 0.001 {
			t.Fatalf("GainFactor(%v) = %.4f，期望 %.4f", c.db, got, c.want)
		}
	}
}

func TestApplyGainScalesAndClamps(t *testing.T) {
	pcm := make([]byte, 6)
	put := func(off int, v int16) { binary.LittleEndian.PutUint16(pcm[off:], uint16(v)) }
	put(0, 1000)
	put(2, -2000)
	put(4, 30000)

	ApplyGain(pcm, 2)
	got := []int16{
		int16(binary.LittleEndian.Uint16(pcm[0:])),
		int16(binary.LittleEndian.Uint16(pcm[2:])),
		int16(binary.LittleEndian.Uint16(pcm[4:])),
	}
	want := []int16{2000, -4000, math.MaxInt16} // 30000×2 溢出，钳制到 32767
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("样本 %d = %d，期望 %d", i, got[i], want[i])
		}
	}

	// factor 为 1 时原样返回（零拷贝路径）。
	orig := append([]byte(nil), pcm...)
	ApplyGain(pcm, 1)
	for i := range orig {
		if pcm[i] != orig[i] {
			t.Fatal("factor=1 不应改动样本")
		}
	}
	// 空输入与半字节输入不 panic。
	ApplyGain(nil, 2)
	ApplyGain([]byte{0x01}, 2)
}
