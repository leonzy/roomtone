package player_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"songloft/internal/player"
)

// newRenameManager 造一个带内存仓储的管理器，并注册一台设备（自动建单机组）。
func newRenameManager(t *testing.T) (*player.Manager, *memStore) {
	t.Helper()
	store := newMemStore()
	m := player.NewManager(player.ManagerDeps{
		Store:  store,
		Bus:    player.NewEventBus(),
		Budget: player.NewBudget(4),
	})
	if err := m.RegisterPlayer(context.Background(), player.Player{ID: "a", Kind: player.KindRAOP, Name: "客厅"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	return m, store
}

// TestRenameGroupValidation 名称与图标校验：空名 / 超长 / 非法图标都拒绝。
func TestRenameGroupValidation(t *testing.T) {
	m, _ := newRenameManager(t)
	ctx := context.Background()
	gid := m.GroupIDOf("a")

	cases := []struct {
		name    string
		group   string
		newName string
		icon    string
		wantErr error
	}{
		{name: "空名", group: gid, newName: "", icon: "", wantErr: player.ErrInvalidName},
		{name: "纯空白", group: gid, newName: "   ", icon: "", wantErr: player.ErrInvalidName},
		{name: "超长", group: gid, newName: strings.Repeat("区", 41), icon: "", wantErr: player.ErrInvalidName},
		{name: "非法图标", group: gid, newName: "客厅", icon: "rocket", wantErr: player.ErrInvalidIcon},
		{name: "组不存在", group: "grp-missing", newName: "客厅", icon: "", wantErr: player.ErrGroupNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := m.RenameGroup(ctx, tc.group, tc.newName, tc.icon); !errors.Is(err, tc.wantErr) {
				t.Fatalf("错误 = %v，期望 %v", err, tc.wantErr)
			}
		})
	}

	// 校验失败不能改动原值。
	g, err := m.GetGroup(gid)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if g.Name != "客厅" || g.Icon != "" {
		t.Fatalf("校验失败后组被改动：name=%q icon=%q", g.Name, g.Icon)
	}
}

// TestRenameGroupUpdatesMemoryAndPersists 正常改名：内存立即生效、事件广播、
// 用同一仓储重建管理器（模拟重启）后名称与图标仍在。
func TestRenameGroupUpdatesMemoryAndPersists(t *testing.T) {
	m, store := newRenameManager(t)
	ctx := context.Background()
	gid := m.GroupIDOf("a")

	events, cancel := m.Bus().Subscribe()
	defer cancel()

	g, err := m.RenameGroup(ctx, gid, "  书房  ", "bookshelf")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if g.Name != "书房" || g.Icon != "bookshelf" {
		t.Fatalf("返回值 = name %q icon %q，期望 书房 / bookshelf", g.Name, g.Icon)
	}

	select {
	case ev := <-events:
		if ev.Type != player.EventGroupChanged || ev.Action != "renamed" || ev.GroupID != gid {
			t.Fatalf("事件 = %+v，期望 group_changed/renamed/%s", ev, gid)
		}
	case <-time.After(time.Second):
		t.Fatal("没有收到 group_changed 事件")
	}

	// 模拟服务重启：新管理器从同一仓储恢复。
	restarted := player.NewManager(player.ManagerDeps{
		Store:  store,
		Bus:    player.NewEventBus(),
		Budget: player.NewBudget(4),
	})
	if err := restarted.Restore(ctx); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := restarted.GetGroup(gid)
	if err != nil {
		t.Fatalf("restart get group: %v", err)
	}
	if got.Name != "书房" || got.Icon != "bookshelf" {
		t.Fatalf("重启后 = name %q icon %q，期望 书房 / bookshelf", got.Name, got.Icon)
	}
}

// TestRenameGroupAllowsEmptyIconAndFortyRunes 空图标表示默认；40 个字符是上限。
func TestRenameGroupAllowsEmptyIconAndFortyRunes(t *testing.T) {
	m, _ := newRenameManager(t)
	ctx := context.Background()
	gid := m.GroupIDOf("a")

	name := strings.Repeat("区", 40)
	g, err := m.RenameGroup(ctx, gid, name, "")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if g.Name != name || g.Icon != "" {
		t.Fatalf("返回值 = name %q icon %q", g.Name, g.Icon)
	}
}

func TestSpeakerRenameSurvivesDiscoveryAndRestart(t *testing.T) {
	m, store := newRenameManager(t)
	ctx := context.Background()
	name := "  书房  "
	if _, err := m.UpdatePlayer(ctx, "a", player.PlayerUpdate{Name: &name}); err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterPlayer(ctx, player.Player{ID: "a", Kind: player.KindRAOP, Name: "EDIFIER advertised name"}); err != nil {
		t.Fatal(err)
	}
	p, _ := m.GetPlayer("a")
	g, _ := m.GetGroup(m.GroupIDOf("a"))
	if p.Name != "书房" || g.Name != "书房" {
		t.Fatalf("rediscovery lost alias: player=%q zone=%q", p.Name, g.Name)
	}
	restarted := player.NewManager(player.ManagerDeps{Store: store, Budget: player.NewBudget(4)})
	if err := restarted.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	p, _ = restarted.GetPlayer("a")
	g, _ = restarted.GetGroup(restarted.GroupIDOf("a"))
	if p.Name != "书房" || g.Name != "书房" {
		t.Fatalf("restart lost alias: player=%q zone=%q", p.Name, g.Name)
	}
}

func TestSpeakerAndSoloZoneShareName(t *testing.T) {
	m, _ := newRenameManager(t)
	ctx := context.Background()
	if _, err := m.RenameGroup(ctx, m.GroupIDOf("a"), "听音区", "speaker"); err != nil {
		t.Fatal(err)
	}
	p, _ := m.GetPlayer("a")
	if p.Name != "听音区" {
		t.Fatalf("zone rename not visible on speaker: %q", p.Name)
	}
	name := "书房音箱"
	if _, err := m.UpdatePlayer(ctx, "a", player.PlayerUpdate{Name: &name}); err != nil {
		t.Fatal(err)
	}
	g, _ := m.GetGroup(m.GroupIDOf("a"))
	if g.Name != "书房音箱" {
		t.Fatalf("speaker rename did not update solo zone: %q", g.Name)
	}
	blank := "   "
	if _, err := m.UpdatePlayer(ctx, "a", player.PlayerUpdate{Name: &blank}); !errors.Is(err, player.ErrInvalidArgument) {
		t.Fatalf("blank speaker name: %v", err)
	}
}

func TestZoneNameSurvivesGrouping(t *testing.T) {
	m, _ := newRenameManager(t)
	ctx := context.Background()
	if err := m.RegisterPlayer(ctx, player.Player{ID: "b", Kind: player.KindRAOP, Name: "Factory B"}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RenameGroup(ctx, m.GroupIDOf("a"), "餐厅", "speaker"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RenameGroup(ctx, m.GroupIDOf("b"), "卧室", "speaker"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddMember(ctx, m.GroupIDOf("a"), "b"); err != nil {
		t.Fatal(err)
	}
	a, _ := m.GetPlayer("a")
	b, _ := m.GetPlayer("b")
	if a.Name != "餐厅" || b.Name != "卧室" {
		t.Fatalf("grouped names: %q %q", a.Name, b.Name)
	}
}
