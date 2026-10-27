package app

import (
	"context"
	"strings"
	"testing"

	"songloft/internal/player"
	"songloft/internal/player/sink/raop"
	"songloft/internal/player/sink/raop2"
)

// TestPlayerSinkBuilderRAOPNegotiation A2P-7：builder 按 Caps.AirPlay2 分派
// V1（raop）/ V2（raop2）客户端；两者 OnControl/Password 均透传。
func TestPlayerSinkBuilderRAOPNegotiation(t *testing.T) {
	b := &playerSinkBuilder{
		deviceControl: func(ctx context.Context, playerID, cmd string, value float64) error {
			return nil
		},
	}

	cases := []struct {
		name     string
		airplay2 bool
		wantV2   bool
	}{
		{"V1 设备（无 _airplay._tcp）", false, false},
		{"V2 设备（Caps.AirPlay2）", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &player.Player{
				ID:       "raop:aa:bb:cc:dd:ee:ff",
				Kind:     player.KindRAOP,
				Address:  "10.0.0.5:7000",
				Password: "pw",
				Caps: player.Caps{
					DeviceID: "aabbccddeeff",
					AirPlay2: c.airplay2,
				},
			}
			s, err := b.Build(context.Background(), p)
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			// Kind 仍为 raop（对客户端零破坏，V1/V2 是同一设备的传输代际）
			if s.Kind() != player.KindRAOP {
				t.Fatalf("Kind = %q，期望 raop", s.Kind())
			}
			// 类型分派断言
			_, isV1 := s.(*raop.Sink)
			_, isV2 := s.(*raop2.Sink)
			if c.wantV2 && !isV2 {
				t.Fatalf("Caps.AirPlay2=true 应返回 raop2.Sink，实际 %T", s)
			}
			if !c.wantV2 && !isV1 {
				t.Fatalf("Caps.AirPlay2=false 应返回 raop.Sink，实际 %T", s)
			}
		})
	}

	// 地址为空 → 明确错误
	p := &player.Player{ID: "x", Kind: player.KindRAOP, Caps: player.Caps{AirPlay2: true}}
	if _, err := b.Build(context.Background(), p); err == nil || !strings.Contains(err.Error(), "地址为空") {
		t.Fatalf("地址为空应报错: %v", err)
	}
}
