package player_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"songloft/internal/player"
)

func strPtr(s string) *string { return &s }

// TestPlayerPasswordPersist A4：密码随 Player 存取——注册带上、重启
// （同一仓储重建管理器）后仍在；JSON 序列化不泄漏密码字段。
func TestPlayerPasswordPersist(t *testing.T) {
	store := newMemStore()
	m := player.NewManager(player.ManagerDeps{
		Store:  store,
		Bus:    player.NewEventBus(),
		Budget: player.NewBudget(4),
	})
	ctx := context.Background()
	if err := m.RegisterPlayer(ctx, player.Player{
		ID: "a", Kind: player.KindRAOP, Name: "客厅", Password: "s3cret",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	p, err := m.GetPlayer("a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if p.Password != "s3cret" {
		t.Fatalf("注册后密码 = %q，期望 s3cret", p.Password)
	}

	// 模拟重启：同一仓储重建管理器并 Restore。
	m2 := player.NewManager(player.ManagerDeps{
		Store:  store,
		Bus:    player.NewEventBus(),
		Budget: player.NewBudget(4),
	})
	if err := m2.Restore(ctx); err != nil {
		t.Fatalf("restore: %v", err)
	}
	p2, err := m2.GetPlayer("a")
	if err != nil {
		t.Fatalf("get after restore: %v", err)
	}
	if p2.Password != "s3cret" {
		t.Fatalf("重启后密码丢失： %q", p2.Password)
	}

	// JSON 序列化不含密码（防泄漏，Password 为 json:"-"）。
	raw, err := json.Marshal(p2)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "s3cret") {
		t.Fatalf("Player JSON 泄漏密码: %s", raw)
	}
}

// TestPlayerPasswordUpdate A4：UpdatePlayer 的 Password 指针语义——
// 非 nil 更新（空串清空），nil 不覆盖。
func TestPlayerPasswordUpdate(t *testing.T) {
	store := newMemStore()
	m := player.NewManager(player.ManagerDeps{
		Store:  store,
		Bus:    player.NewEventBus(),
		Budget: player.NewBudget(4),
	})
	ctx := context.Background()
	if err := m.RegisterPlayer(ctx, player.Player{
		ID: "a", Kind: player.KindRAOP, Name: "客厅", Password: "old",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// nil 不覆盖（其它字段更新时密码保留）。
	if _, err := m.UpdatePlayer(ctx, "a", player.PlayerUpdate{Name: strPtr("书房")}); err != nil {
		t.Fatalf("update name: %v", err)
	}
	p, _ := m.GetPlayer("a")
	if p.Password != "old" || p.Name != "书房" {
		t.Fatalf("nil 密码被覆盖: %+v", p)
	}

	// 非 nil 更新。
	if _, err := m.UpdatePlayer(ctx, "a", player.PlayerUpdate{Password: strPtr("new")}); err != nil {
		t.Fatalf("update password: %v", err)
	}
	p, _ = m.GetPlayer("a")
	if p.Password != "new" {
		t.Fatalf("密码未更新: %q", p.Password)
	}

	// 空串清空。
	if _, err := m.UpdatePlayer(ctx, "a", player.PlayerUpdate{Password: strPtr("")}); err != nil {
		t.Fatalf("clear password: %v", err)
	}
	p, _ = m.GetPlayer("a")
	if p.Password != "" {
		t.Fatalf("密码未清空: %q", p.Password)
	}
	_ = time.Second
}
