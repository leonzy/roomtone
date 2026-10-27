package player_test

import (
	"context"
	"testing"
	"time"

	"songloft/internal/player"
)

// TestManagerDeviceControl A2/A3：设备侧反控入口（DACP / RAAT 共用）把
// 命令映射到引擎传输操作。
func TestManagerDeviceControl(t *testing.T) {
	m, _, _ := newTestManager(t, "a")
	ctx := context.Background()
	gid := m.GroupIDOf("a")
	e, err := m.Engine(gid)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if err := e.Load(ctx, []int64{11, 12}, 0, 0); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := e.Play(ctx); err != nil {
		t.Fatalf("play: %v", err)
	}
	// 等待真正出声（loading → playing），对齐真实场景：
	// 音箱按键只在有声音时可按时到达。
	waitFor(t, "引擎进入 playing", 4*time.Second, func() bool {
		return e.Status().State == player.StatePlaying
	})

	// pause → 引擎暂停
	if err := m.DeviceControl(ctx, "a", "pause", 0); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if s := e.Status().State; s != player.StatePaused {
		t.Fatalf("pause 后状态 = %q，期望 paused", s)
	}
	// play → 恢复（重新起播，等出声）
	if err := m.DeviceControl(ctx, "a", "play", 0); err != nil {
		t.Fatalf("play: %v", err)
	}
	waitFor(t, "恢复后 playing", 4*time.Second, func() bool {
		return e.Status().State == player.StatePlaying
	})
	// nextitem → 当前曲目 +1
	if err := m.DeviceControl(ctx, "a", "nextitem", 0); err != nil {
		t.Fatalf("nextitem: %v", err)
	}
	if id := e.Status().SongID; id != 12 {
		t.Fatalf("nextitem 后当前曲目 = %d，期望 12", id)
	}
	// previtem → 回到 11（立即切歌，等出声）
	if err := m.DeviceControl(ctx, "a", "previtem", 0); err != nil {
		t.Fatalf("previtem: %v", err)
	}
	if id := e.Status().SongID; id != 11 {
		t.Fatalf("previtem 后当前曲目 = %d，期望 11", id)
	}
	waitFor(t, "previtem 后 playing", 4*time.Second, func() bool {
		return e.Status().State == player.StatePlaying
	})
	// seek → 位置跳转（重建解码环，等出声再验证位置）
	if err := m.DeviceControl(ctx, "a", "seek", 42); err != nil {
		t.Fatalf("seek: %v", err)
	}
	waitFor(t, "seek 后 playing", 4*time.Second, func() bool {
		return e.Status().State == player.StatePlaying
	})
	if pos := e.Status().Position; pos < 41 || pos > 43 {
		t.Fatalf("seek(42) 后位置 = %v，期望 ≈42s", pos)
	}
	// setvolume 0.5 → Volume == 50
	if err := m.DeviceControl(ctx, "a", "setvolume", 0.5); err != nil {
		t.Fatalf("setvolume: %v", err)
	}
	p, err := m.GetPlayer("a")
	if err != nil {
		t.Fatalf("getplayer: %v", err)
	}
	if p.Volume != 50 {
		t.Fatalf("setvolume(0.5) 后 Volume = %d，期望 50", p.Volume)
	}
	// preventplayback → 暂停（对齐 Roon 语义）
	if err := m.DeviceControl(ctx, "a", "preventplayback", 0); err != nil {
		t.Fatalf("preventplayback: %v", err)
	}
	if s := e.Status().State; s != player.StatePaused {
		t.Fatalf("preventplayback 后状态 = %q，期望 paused", s)
	}
	// stop → 停止
	if err := m.DeviceControl(ctx, "a", "play", 0); err != nil {
		t.Fatalf("play: %v", err)
	}
	waitFor(t, "再次播放", 4*time.Second, func() bool {
		return e.Status().State == player.StatePlaying
	})
	if err := m.DeviceControl(ctx, "a", "stop", 0); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if s := e.Status().State; s != player.StateIdle {
		t.Fatalf("stop 后状态 = %q，期望 idle", s)
	}
	// 未知命令 → 明确错误，不 panic
	if err := m.DeviceControl(ctx, "a", "bogus", 0); err == nil {
		t.Fatal("未知命令应当报错")
	}
	// 不存在的设备 → ErrPlayerNotFound
	if err := m.DeviceControl(ctx, "ghost", "pause", 0); err != player.ErrPlayerNotFound {
		t.Fatalf("不存在的设备应当返回 ErrPlayerNotFound，得到 %v", err)
	}
	_ = time.Second
}
