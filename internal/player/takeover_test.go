package player_test

import (
	"context"
	"testing"

	"songloft/internal/player"
)

// TestUpdatePlayerTakeoverPersists 接管行为只对 local 生效，且重新注册不能覆盖。
func TestUpdatePlayerTakeoverPersists(t *testing.T) {
	m, _, _ := newTestManager(t)
	ctx := context.Background()
	if err := m.RegisterPlayer(ctx, player.Player{
		ID: "local-1", Kind: player.KindLocal, Name: "本机",
	}); err != nil {
		t.Fatalf("register local: %v", err)
	}

	follow := player.TakeoverFollow
	p, err := m.UpdatePlayer(ctx, "local-1", player.PlayerUpdate{Takeover: &follow})
	if err != nil {
		t.Fatalf("update takeover: %v", err)
	}
	if p.Takeover != player.TakeoverFollow {
		t.Fatalf("takeover = %q，期望 follow", p.Takeover)
	}

	// 客户端重连会重新 register_player；用户设置必须保留。
	if err := m.RegisterPlayer(ctx, player.Player{
		ID: "local-1", Kind: player.KindLocal, Name: "本机",
	}); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	got, err := m.GetPlayer("local-1")
	if err != nil {
		t.Fatalf("get player: %v", err)
	}
	if got.Takeover != player.TakeoverFollow {
		t.Fatalf("重新注册后 takeover = %q，期望 follow", got.Takeover)
	}
}

// TestUpdatePlayerTakeoverRejectsInvalid 非法值返回参数错误，不能落库。
func TestUpdatePlayerTakeoverRejectsInvalid(t *testing.T) {
	m, _, _ := newTestManager(t)
	ctx := context.Background()
	if err := m.RegisterPlayer(ctx, player.Player{
		ID: "local-1", Kind: player.KindLocal, Name: "本机",
	}); err != nil {
		t.Fatalf("register local: %v", err)
	}
	bad := player.TakeoverBehavior("stop")
	if _, err := m.UpdatePlayer(ctx, "local-1", player.PlayerUpdate{Takeover: &bad}); err == nil {
		t.Fatal("非法 takeover 应返回错误")
	}
	got, _ := m.GetPlayer("local-1")
	if got.Takeover != player.TakeoverPause {
		t.Fatalf("非法值不应落库，实际 %q", got.Takeover)
	}
}
