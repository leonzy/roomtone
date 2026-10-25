package player

import (
	"errors"
	"testing"
	"time"
)

// fakeClock 让位置推进可预测：只读不睡眠。
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestTransport() (*Transport, *fakeClock) {
	clk := &fakeClock{t: time.Unix(1700000000, 0)}
	tr := NewTransport()
	tr.SetClock(clk.Now)
	return tr, clk
}

func TestTransportPlayPauseKeepsPosition(t *testing.T) {
	tr, clk := newTestTransport()
	if err := tr.Load(1, 0); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := tr.MarkBuffering(); err != nil {
		t.Fatalf("MarkBuffering: %v", err)
	}
	if !tr.ReportWatermark(tr.target) {
		t.Fatal("水位达标应起播")
	}
	if tr.State() != StatePlaying {
		t.Fatalf("state = %s", tr.State())
	}
	clk.Advance(3 * time.Second)
	if pos := tr.Position(); pos != 3*time.Second {
		t.Fatalf("position = %v, want 3s", pos)
	}
	if err := tr.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	clk.Advance(10 * time.Second) // 暂停期间不推进
	if pos := tr.Position(); pos != 3*time.Second {
		t.Fatalf("暂停后 position = %v, want 3s", pos)
	}
	if err := tr.Play(); err != nil {
		t.Fatalf("Play: %v", err)
	}
	clk.Advance(time.Second)
	if pos := tr.Position(); pos != 4*time.Second {
		t.Fatalf("恢复后 position = %v, want 4s", pos)
	}
}

func TestTransportWatermarkGate(t *testing.T) {
	tr, _ := newTestTransport()
	_ = tr.Load(1, 0)
	_ = tr.MarkBuffering()
	if tr.ReportWatermark(tr.target / 2) {
		t.Fatal("水位不足不应起播")
	}
	if tr.State() != StateBuffering {
		t.Fatalf("state = %s", tr.State())
	}
	if !tr.ReportWatermark(tr.target + time.Millisecond) {
		t.Fatal("水位达标应起播")
	}
}

func TestTransportSeekResetsBase(t *testing.T) {
	tr, clk := newTestTransport()
	_ = tr.Load(1, 0)
	_ = tr.MarkBuffering()
	tr.ReportWatermark(tr.target)
	clk.Advance(5 * time.Second)
	tr.Seek(60 * time.Second)
	if pos := tr.Position(); pos != 60*time.Second {
		t.Fatalf("seek 后 position = %v", pos)
	}
	clk.Advance(2 * time.Second)
	if pos := tr.Position(); pos != 62*time.Second {
		t.Fatalf("seek 后继续播放 position = %v", pos)
	}
	if tr.State() != StatePlaying {
		t.Fatalf("seek 不应改变播放状态: %s", tr.State())
	}
}

func TestTransportErrorAndRecover(t *testing.T) {
	tr, _ := newTestTransport()
	_ = tr.Load(1, 0)
	tr.Fail(errors.New("decoder boom"))
	if tr.State() != StateError || tr.Error() != "decoder boom" {
		t.Fatalf("state=%s err=%q", tr.State(), tr.Error())
	}
	if err := tr.Play(); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("错误态 Play 应返回 ErrInvalidTransition，得到 %v", err)
	}
	tr.Recover()
	if tr.State() != StateIdle || tr.SongID() != 0 {
		t.Fatalf("Recover 后 state=%s song=%d", tr.State(), tr.SongID())
	}
}

func TestTransportInvalidTransitions(t *testing.T) {
	tr, _ := newTestTransport()
	if err := tr.MarkBuffering(); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("idle 下 MarkBuffering 应失败: %v", err)
	}
	if err := tr.Pause(); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("idle 下 Pause 应失败: %v", err)
	}
}
