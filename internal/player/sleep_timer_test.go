package player_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"songloft/internal/player"
)

// TestSleepTimerSetAndCancel 设置后能读到截止时刻，0 取消后读不到。
func TestSleepTimerSetAndCancel(t *testing.T) {
	m, _, _ := newTestManager(t, "a")
	gid := m.GroupIDOf("a")
	defer func() { _ = m.SetSleepTimer(gid, 0) }()

	if _, ok := m.SleepTimer(gid); ok {
		t.Fatal("初始不应有睡眠定时器")
	}
	if err := m.SetSleepTimer(gid, time.Minute); err != nil {
		t.Fatalf("设置睡眠定时器: %v", err)
	}
	deadline, ok := m.SleepTimer(gid)
	if !ok {
		t.Fatal("设置后 SleepTimer 应返回 ok=true")
	}
	if remaining := time.Until(deadline); remaining < 55*time.Second || remaining > 65*time.Second {
		t.Fatalf("剩余时间 = %v，期望约 1 分钟", remaining)
	}

	if err := m.SetSleepTimer(gid, 0); err != nil {
		t.Fatalf("取消睡眠定时器: %v", err)
	}
	if _, ok := m.SleepTimer(gid); ok {
		t.Fatal("取消后 SleepTimer 应返回 ok=false")
	}
}

// TestSleepTimerUnknownGroup 不存在的组返回 ErrGroupNotFound。
func TestSleepTimerUnknownGroup(t *testing.T) {
	m, _, _ := newTestManager(t, "a")
	if err := m.SetSleepTimer("grp-missing", time.Minute); !errors.Is(err, player.ErrGroupNotFound) {
		t.Fatalf("错误 = %v，期望 ErrGroupNotFound", err)
	}
}

// TestSleepTimerCancelPreventsPause 取消后旧定时器不能再暂停播放。
func TestSleepTimerCancelPreventsPause(t *testing.T) {
	m, _, _ := newTestManager(t, "a")
	ctx := context.Background()
	gid := m.GroupIDOf("a")
	e, err := m.Engine(gid)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if err := e.Load(ctx, []int64{1}, 0, 0); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := e.Play(ctx); err != nil {
		t.Fatalf("play: %v", err)
	}
	waitFor(t, "起播", 2*time.Second, func() bool {
		return e.Status().State == player.StatePlaying
	})

	if err := m.SetSleepTimer(gid, 50*time.Millisecond); err != nil {
		t.Fatalf("设置睡眠定时器: %v", err)
	}
	if err := m.SetSleepTimer(gid, 0); err != nil {
		t.Fatalf("取消睡眠定时器: %v", err)
	}
	time.Sleep(120 * time.Millisecond)
	if got := e.Status().State; got != player.StatePlaying {
		t.Fatalf("取消后状态 = %s，期望仍在 playing", got)
	}
}

// TestSleepTimerPausesAtDeadline 到点后走引擎 Pause 路径，状态变 paused 并广播 state。
func TestSleepTimerPausesAtDeadline(t *testing.T) {
	m, _, _ := newTestManager(t, "a")
	ctx := context.Background()
	gid := m.GroupIDOf("a")
	e, err := m.Engine(gid)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if err := e.Load(ctx, []int64{1}, 0, 0); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := e.Play(ctx); err != nil {
		t.Fatalf("play: %v", err)
	}
	waitFor(t, "起播", 2*time.Second, func() bool {
		return e.Status().State == player.StatePlaying
	})

	events, cancel := m.Bus().Subscribe()
	defer cancel()

	if err := m.SetSleepTimer(gid, 50*time.Millisecond); err != nil {
		t.Fatalf("设置睡眠定时器: %v", err)
	}
	waitFor(t, "到点暂停", 2*time.Second, func() bool {
		return e.Status().State == player.StatePaused
	})
	if _, ok := m.SleepTimer(gid); ok {
		t.Fatal("定时器触发后应从 map 中移除")
	}

	timeout := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Type == player.EventState && ev.GroupID == gid && ev.Status == string(player.StatePaused) {
				return
			}
		case <-timeout:
			t.Fatal("未收到 paused 的 state 事件")
		}
	}
}
