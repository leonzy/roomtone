package player_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"songloft/internal/player"
)

// TestDeleteLastPlayerB0804 删最后一台设备：不 panic、列表为空、组也清空。
//
// 这是 B8-04 的第一个边界。用户买第一台音箱之前系统里就是"零设备零组"，
// 删到空必须能回到这个状态，而不是留下一个指向已删设备的幽灵组。
func TestDeleteLastPlayerB0804(t *testing.T) {
	m, builder, _ := newTestManager(t, "a")
	ctx := context.Background()
	gid := m.GroupIDOf("a")

	if err := m.DeletePlayer(ctx, "a"); err != nil {
		t.Fatalf("删除唯一设备失败：%v", err)
	}
	if got := m.ListPlayers(); len(got) != 0 {
		t.Fatalf("删完还剩 %d 台设备", len(got))
	}
	if got := m.ListGroups(); len(got) != 0 {
		t.Fatalf("删完还剩 %d 个组", len(got))
	}
	if _, err := m.GetGroup(gid); !errors.Is(err, player.ErrGroupNotFound) {
		t.Fatalf("旧组应不存在，实际 err=%v", err)
	}
	// sink 必须被关掉，否则真机上连接会挂着。
	if !builder.get("a").IsClosed() {
		t.Fatal("设备删除后 sink 未关闭")
	}
	// 再删一次要报不存在，不能静默成功。
	if err := m.DeletePlayer(ctx, "a"); !errors.Is(err, player.ErrPlayerNotFound) {
		t.Fatalf("重复删除应返回 ErrPlayerNotFound，实际 %v", err)
	}
}

// TestDeletePlayingGroupB0804 删正在播放的组：停播、释放预算、成员回单机组。
//
// 第二个边界。删组必须先把引擎停掉再摘成员，否则分发协程会往已关闭的
// sink 写帧；解码槽位不还的话，删两个组就会假装"同时播放数已满"。
func TestDeletePlayingGroupB0804(t *testing.T) {
	m, builder, _ := newTestManager(t, "a", "b")
	ctx := context.Background()
	gid := m.GroupIDOf("a")
	if _, err := m.AddMember(ctx, gid, "b"); err != nil {
		t.Fatalf("add member: %v", err)
	}
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
	waitFor(t, "起播", 2*time.Second, func() bool { return builder.get("a").FrameCount() >= 3 })

	if err := m.DeleteGroup(ctx, gid); err != nil {
		t.Fatalf("删除正在播放的组失败：%v", err)
	}
	if _, err := m.GetGroup(gid); !errors.Is(err, player.ErrGroupNotFound) {
		t.Fatalf("组应已删除，实际 err=%v", err)
	}
	// 两台设备都还在，并且各自回到单机组。
	for _, id := range []string{"a", "b"} {
		if _, err := m.GetPlayer(id); err != nil {
			t.Fatalf("设备 %s 不该被删掉：%v", id, err)
		}
		if solo := m.GroupIDOf(id); solo == "" || solo == gid {
			t.Fatalf("设备 %s 没有回到单机组（group=%q）", id, solo)
		}
	}
	// 预算必须还回来：再建一个组并播放应当成功。
	ng, err := m.CreateGroup(ctx, "new", []string{"a", "b"}, "a")
	if err != nil {
		t.Fatalf("重新建组失败：%v", err)
	}
	ne, err := m.Engine(ng.ID)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if err := ne.Load(ctx, []int64{1}, 0, 0); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := ne.Play(ctx); err != nil {
		t.Fatalf("删除组后解码槽位没有释放：%v", err)
	}
}

// TestDeleteGroupInPartyB0804 派对中的组不能删：返回 409 语义的错误。
func TestDeleteGroupInPartyB0804(t *testing.T) {
	m, _ := newPartyManager(t, "a", "b")
	ctx := context.Background()
	ga := m.GroupIDOf("a")
	gb := m.GroupIDOf("b")
	if _, err := m.CreateParty(ctx, "p", []string{ga, gb}); err != nil {
		t.Fatalf("create party: %v", err)
	}
	if err := m.DeleteGroup(ctx, ga); !errors.Is(err, player.ErrGroupInParty) {
		t.Fatalf("派对中的组应拒绝删除，实际 %v", err)
	}
	if _, err := m.GetGroup(ga); err != nil {
		t.Fatalf("拒绝后组应仍然存在：%v", err)
	}
}

// TestSetMasterB0804 换主控：播放中切换不断音、组音量基准跟着换。
func TestSetMasterB0804(t *testing.T) {
	m, builder, _ := newTestManager(t, "a", "b")
	ctx := context.Background()
	gid := m.GroupIDOf("a")
	if _, err := m.AddMember(ctx, gid, "b"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	e, _ := m.Engine(gid)
	if err := e.Load(ctx, []int64{1}, 0, 0); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := e.Play(ctx); err != nil {
		t.Fatalf("play: %v", err)
	}
	a := builder.get("a")
	waitFor(t, "起播", 2*time.Second, func() bool { return a.FrameCount() >= 3 })
	before := a.FrameCount()

	g, err := m.SetMaster(ctx, gid, "b")
	if err != nil {
		t.Fatalf("换主控失败：%v", err)
	}
	if g.MasterID != "b" {
		t.Fatalf("主控应变成 b，实际 %q", g.MasterID)
	}
	// 播放不受影响：帧继续推进，且没有重建 sink。
	waitFor(t, "换主控后继续推进", time.Second, func() bool { return a.FrameCount() > before+2 })
	if got := a.SilentFrames(); got > 3 {
		t.Fatalf("换主控导致 %d 帧欠载静音", got)
	}
	// 不在组内的设备要拒绝。
	if _, err := m.SetMaster(ctx, gid, "nobody"); !errors.Is(err, player.ErrPlayerNotFound) {
		t.Fatalf("非成员应被拒绝，实际 %v", err)
	}
}

// TestStandbyExcludesFromSyncB0805 待机成员立刻停止收帧，唤醒后重新接入。
func TestStandbyExcludesFromSyncB0805(t *testing.T) {
	m, builder, _ := newTestManager(t, "a", "b")
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
	b := builder.get("b")
	waitFor(t, "b 起播", 2*time.Second, func() bool { return b.FrameCount() >= 3 })

	p, err := m.SetPower(ctx, "b", false)
	if err != nil {
		t.Fatalf("待机失败：%v", err)
	}
	if !p.Standby {
		t.Fatal("待机后 standby 字段应为 true")
	}
	// 记下此刻帧数，之后不应再增长。
	before := b.FrameCount()
	time.Sleep(120 * time.Millisecond)
	if got := b.FrameCount(); got > before+1 {
		t.Fatalf("待机成员仍在收帧：%d → %d", before, got)
	}
	if !b.IsStopped() {
		t.Fatal("待机时 sink 应被 Stop")
	}

	p, err = m.SetPower(ctx, "b", true)
	if err != nil {
		t.Fatalf("唤醒失败：%v", err)
	}
	if p.Standby {
		t.Fatal("唤醒后 standby 应为 false")
	}
	waitFor(t, "唤醒后重新收帧", 2*time.Second, func() bool { return b.FrameCount() > before+2 })
}
