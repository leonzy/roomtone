package player_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"songloft/internal/player"
)

// manualClock 手动推进的时钟，用来快进对齐窗口。
type manualClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// TestMemberSyncingOnJoinAndWake 入组 / 唤醒后标"对齐中"，窗口过后自动消失。
//
// 校正环每 950ms 采样一次，一轮内收敛；窗口取 3 个周期。这里用注入的时钟
// 直接跨过窗口，避免测试里真等 3 秒。
func TestMemberSyncingOnJoinAndWake(t *testing.T) {
	clock := &manualClock{t: time.Unix(1700000000, 0)}
	m, _, _ := newTestManagerWithClock(t, clock.Now, "a", "b")
	ctx := context.Background()
	gid := m.GroupIDOf("a")

	if _, err := m.AddMember(ctx, gid, "b"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if !syncingOf(t, m, gid, "b") {
		t.Fatal("刚入组的设备应标 syncing")
	}
	if syncingOf(t, m, gid, "a") {
		t.Fatal("老成员不应标 syncing")
	}

	// 跨过 3 个校正周期，标记必须自动消失，否则会一直显示"对齐中"。
	clock.advance(3*player.DefaultSyncInterval + 1)
	if syncingOf(t, m, gid, "b") {
		t.Fatal("窗口过后 syncing 应自动清除")
	}

	// 待机再唤醒：重新进入对齐窗口。
	if _, err := m.SetPower(ctx, "b", false); err != nil {
		t.Fatalf("standby: %v", err)
	}
	if _, err := m.SetPower(ctx, "b", true); err != nil {
		t.Fatalf("wake: %v", err)
	}
	if !syncingOf(t, m, gid, "b") {
		t.Fatal("唤醒后应重新标 syncing")
	}
}

func syncingOf(t *testing.T, m *player.Manager, groupID, playerID string) bool {
	t.Helper()
	g, err := m.GetGroup(groupID)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	for _, c := range g.MemberCaps {
		if c.PlayerID == playerID {
			return c.Syncing
		}
	}
	t.Fatalf("成员 %s 不在组 %s 的 member_caps 里", playerID, groupID)
	return false
}
