package player_test

import (
	"context"
	"testing"
	"time"

	"songloft/internal/player"
)

// TestNextDuringPlaybackSkips 播放中按下一首立即切歌（回归：此前
// startLocked 因 running=true 提前返回，旧解码环播完当前曲后 EOF 处
// queue.Next(true) 再推一格，按下一首反而整组停播）。
func TestNextDuringPlaybackSkips(t *testing.T) {
	// 600s 长曲：短于推送深度（2s）的歌会瞬间 EOF，等 playing 时可能已切到下一首。
	m, _, _ := newTestManager(t, "a")
	ctx := context.Background()
	e, err := m.Engine(m.GroupIDOf("a"))
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if err := e.Load(ctx, []int64{11, 12, 13}, 0, 0); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := e.Play(ctx); err != nil {
		t.Fatalf("play: %v", err)
	}
	waitFor(t, "playing", 4*time.Second, func() bool {
		return e.Status().State == player.StatePlaying
	})
	if err := e.Next(ctx, false); err != nil {
		t.Fatalf("next: %v", err)
	}
	// 立即切到 12 并继续播放（不等 11 播完）。
	if id := e.Status().SongID; id != 12 {
		t.Fatalf("next 后当前曲目 = %d，期望 12", id)
	}
	waitFor(t, "next 后 playing", 4*time.Second, func() bool {
		return e.Status().State == player.StatePlaying
	})
	// 再按一次到 13。
	if err := e.Next(ctx, false); err != nil {
		t.Fatalf("next2: %v", err)
	}
	if id := e.Status().SongID; id != 13 {
		t.Fatalf("next2 后当前曲目 = %d，期望 13", id)
	}
	waitFor(t, "next2 后 playing", 4*time.Second, func() bool {
		return e.Status().State == player.StatePlaying
	})
}

// TestSeekDuringLoadingRecovers 缓冲期（loading）seek 不再卡死（回归：
// wasPlaying 不含 loading，stopLocked 杀掉 run 协程后无人重启）。
// 用 600s 长曲：短于推送深度（2s）的歌会瞬间 EOF，seek 时已不在 loading。
func TestSeekDuringLoadingRecovers(t *testing.T) {
	m, _, _ := newTestManager(t, "a")
	ctx := context.Background()
	e, err := m.Engine(m.GroupIDOf("a"))
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if err := e.Load(ctx, []int64{11}, 0, 0); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := e.Play(ctx); err != nil {
		t.Fatalf("play: %v", err)
	}
	// 不等 playing，趁 loading/buffering 窗口 seek。
	if err := e.Seek(ctx, 300*time.Millisecond); err != nil {
		t.Fatalf("seek: %v", err)
	}
	waitFor(t, "seek 后 playing", 4*time.Second, func() bool {
		return e.Status().State == player.StatePlaying
	})
}
