package player_test

import (
	"context"
	"testing"
	"time"

	"songloft/internal/player"
)

// TestMetricsSnapshotB0803 在播时应能看到槽位占用、owner 明细与状态。
func TestMetricsSnapshotB0803(t *testing.T) {
	m, _, _ := newTestManager(t, "a", "b")
	ctx := context.Background()
	gid := m.GroupIDOf("a")
	if _, err := m.AddMember(ctx, gid, "b"); err != nil {
		t.Fatalf("add: %v", err)
	}
	e, _ := m.Engine(gid)
	if err := e.Load(ctx, []int64{1}, 0, 0); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := e.Play(ctx); err != nil {
		t.Fatalf("play: %v", err)
	}
	waitFor(t, "起播", 2*time.Second, func() bool {
		return m.MetricsSnapshot().Owners[0].State == "playing"
	})

	snap := m.MetricsSnapshot()
	if snap.DecodeSlotsUsed != 1 {
		t.Fatalf("解码槽位 = %d，期望 1", snap.DecodeSlotsUsed)
	}
	if snap.DecodeSlotsMax < 1 {
		t.Fatalf("槽位上限 = %d", snap.DecodeSlotsMax)
	}
	if snap.Players != 2 || snap.Groups != 1 {
		t.Fatalf("规模 = %d 设备 / %d 组", snap.Players, snap.Groups)
	}
	if len(snap.Owners) != 1 {
		t.Fatalf("owner 明细 %d 条，期望 1", len(snap.Owners))
	}
	o := snap.Owners[0]
	if o.GroupID != gid || o.Owner != "group:"+gid {
		t.Fatalf("owner 归属不对：%+v", o)
	}
	if o.Targets != 2 {
		t.Fatalf("推送目标 %d，期望 2", o.Targets)
	}
}

// TestMetricsCountersB0803 计数器累加与偏差取绝对值。
func TestMetricsCountersB0803(t *testing.T) {
	m, _, _ := newTestManager(t)
	mt := m.Metrics()
	mt.AddUnderrun(3)
	mt.AddUnderrun(0) // 0 不计
	mt.AddDroppedFrames(2)
	mt.AddResync()
	mt.ObserveDrift("group:x", -42*time.Millisecond)

	snap := m.MetricsSnapshot()
	if snap.UnderrunsTotal != 3 {
		t.Fatalf("欠载 = %d，期望 3", snap.UnderrunsTotal)
	}
	if snap.DroppedFramesTotal != 2 {
		t.Fatalf("丢帧 = %d，期望 2", snap.DroppedFramesTotal)
	}
	if snap.ResyncsTotal != 1 {
		t.Fatalf("重挂 = %d，期望 1", snap.ResyncsTotal)
	}
	if got := mt.DriftMs("group:x"); got != 42 {
		t.Fatalf("偏差 = %dms，期望 42（负数取绝对值）", got)
	}
	// 未知 owner 返回 0，不能拿别人的偏差冒充。
	if got := mt.DriftMs("group:nope"); got != 0 {
		t.Fatalf("未知 owner 偏差 = %d", got)
	}
}

// TestMetricsNilSafe 指标对象可为 nil（精简路径不该因为"没接指标"而 panic）。
func TestMetricsNilSafe(t *testing.T) {
	var mt *player.Metrics
	mt.AddUnderrun(1)
	mt.AddDroppedFrames(1)
	mt.AddResync()
	mt.ObserveDrift("x", time.Second)
	if got := mt.DriftMs("x"); got != 0 {
		t.Fatalf("nil 指标读偏差 = %d", got)
	}
}
