package local

import (
	"sync"
	"testing"

	"songloft/internal/player/sink"
)

type recCtl struct {
	mu   sync.Mutex
	cmds []Command
}

func (c *recCtl) SendCommand(cmd Command) error {
	c.mu.Lock()
	c.cmds = append(c.cmds, cmd)
	c.mu.Unlock()
	return nil
}

func (c *recCtl) last() Command {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.cmds) == 0 {
		return Command{}
	}
	return c.cmds[len(c.cmds)-1]
}

// B6-06：EQ 曲线下发给客户端时，applied_by 必须是 client。
//
// 本机是自拉流，服务端拿不到它的 PCM；写 server 会让客户端以为
// 服务端已经处理过而不应用，用户听到的是一条没生效的曲线。
func TestSetEQSendsClientApplied(t *testing.T) {
	ctl := &recCtl{}
	s := New("phone", ctl, sink.Caps{})
	eq := sink.EQSettings{
		Enabled:   true,
		Bands:     []float64{3, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		Preamp:    -3.1,
		AppliedBy: sink.EQAppliedByClient,
	}
	if err := s.SetEQ(eq); err != nil {
		t.Fatalf("set eq: %v", err)
	}
	got := ctl.last()
	if got.Cmd != "eq" || got.EQ == nil {
		t.Fatalf("应下发 eq 指令，实际 %+v", got)
	}
	if got.EQ.AppliedBy != sink.EQAppliedByClient {
		t.Fatalf("applied_by 应为 client，实际 %q", got.EQ.AppliedBy)
	}
	if !got.EQ.Enabled || got.EQ.Preamp != -3.1 || len(got.EQ.Bands) != 10 {
		t.Fatalf("曲线内容不对：%+v", got.EQ)
	}
	if s.EQ().AppliedBy != sink.EQAppliedByClient {
		t.Fatal("sink 应记住已下发的曲线")
	}
}

// load 指令要带上 EQ：客户端重连后不需要额外拉一次状态。
func TestOpenCarriesEQ(t *testing.T) {
	ctl := &recCtl{}
	s := New("phone", ctl, sink.Caps{})
	if err := s.SetEQ(sink.EQSettings{Enabled: true, Bands: make([]float64, 10), AppliedBy: sink.EQAppliedByClient}); err != nil {
		t.Fatalf("set eq: %v", err)
	}
	if err := s.Open(nil, sink.StreamPlan{SongID: 7, Format: sink.Format{Codec: sink.CodecFLAC}}); err != nil {
		t.Fatalf("open: %v", err)
	}
	got := ctl.last()
	if got.Cmd != "load" || got.EQ == nil || got.SongID != 7 {
		t.Fatalf("load 应携带 EQ 与曲目：%+v", got)
	}
}

func TestValidateCommandAcceptsEQ(t *testing.T) {
	if err := ValidateCommand("eq"); err != nil {
		t.Fatalf("eq 是合法指令：%v", err)
	}
	if err := ValidateCommand("nope"); err == nil {
		t.Fatal("未知指令必须报错")
	}
}
