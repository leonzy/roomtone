package player

import (
	"log/slog"
	"testing"

	"songloft/internal/player/sink"
	"songloft/internal/player/sink/local"
)

type eqRecCtl struct{ cmds []local.Command }

func (c *eqRecCtl) SendCommand(cmd local.Command) error {
	c.cmds = append(c.cmds, cmd)
	return nil
}

// B6-06：控制型 sink 拿到的 EQ 必须标 client，且曲线与服务端同一份口径。
func TestSyncControlEQSendsClientApplied(t *testing.T) {
	ctl := &eqRecCtl{}
	s := local.New("phone", ctl, sink.Caps{})
	m := &Manager{deps: ManagerDeps{Logger: slog.Default()}}
	p := &Player{
		ID:   "phone",
		Kind: KindLocal,
		EQ:   EQProfile{Enabled: true, Bands: []float64{3, 0, 0, 0, 0, 0, 0, 0, 0, 0}, Preamp: -3.1},
	}
	m.syncControlEQ(s, p)
	if len(ctl.cmds) != 1 {
		t.Fatalf("应下发一条 eq 指令，实际 %d", len(ctl.cmds))
	}
	got := ctl.cmds[0]
	if got.Cmd != "eq" || got.EQ == nil {
		t.Fatalf("指令不对：%+v", got)
	}
	if got.EQ.AppliedBy != sink.EQAppliedByClient {
		t.Fatalf("applied_by 应为 client，实际 %q", got.EQ.AppliedBy)
	}
	if !got.EQ.Enabled || got.EQ.Bands[0] != 3 || got.EQ.Preamp != -3.1 {
		t.Fatalf("曲线不对：%+v", got.EQ)
	}
}

// 关闭的 EQ 也要下发：客户端必须知道"服务端没开 EQ"，否则会沿用上次的曲线。
func TestSyncControlEQDisabled(t *testing.T) {
	ctl := &eqRecCtl{}
	s := local.New("phone", ctl, sink.Caps{})
	m := &Manager{deps: ManagerDeps{Logger: slog.Default()}}
	p := &Player{ID: "phone", Kind: KindLocal, EQ: EQProfile{Bands: make([]float64, EQBandCount)}}
	m.syncControlEQ(s, p)
	if len(ctl.cmds) != 1 || ctl.cmds[0].EQ == nil {
		t.Fatalf("应下发一条 eq 指令：%+v", ctl.cmds)
	}
	if ctl.cmds[0].EQ.Enabled {
		t.Fatal("flat 曲线应下发 enabled=false")
	}
}

// 非控制型 sink 不实现 SetEQ，不能被误判。
func TestSyncControlEQIgnoresOtherSinks(t *testing.T) {
	m := &Manager{deps: ManagerDeps{Logger: slog.Default()}}
	m.syncControlEQ(nil, &Player{ID: "x"}) // 不应 panic
}
