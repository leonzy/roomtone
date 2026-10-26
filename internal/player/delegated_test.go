package player

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"songloft/internal/player/sink"
)

// ctrlSink 控制型 sink：只收指令、不接收 PCM，模拟本机客户端。
//
// 它刻意**不实现** sink.PushSink——这正是线上 local sink 的形态，
// 也是 rebuildSinks 把它排除在分发列表之外的原因。
type ctrlSink struct {
	id string

	mu     sync.Mutex
	opened []sink.StreamPlan
	starts int
	stops  int
	pauses int
	pos    time.Duration
	hasPos bool
	events chan sink.Event
}

func newCtrlSink(id string) *ctrlSink {
	return &ctrlSink{id: id, events: make(chan sink.Event, 8)}
}

func (c *ctrlSink) ID() string                { return c.id }
func (c *ctrlSink) Kind() sink.Kind           { return sink.KindLocal }
func (c *ctrlSink) Caps() sink.Caps           { return sink.Caps{} }
func (c *ctrlSink) Seek(uint64) error         { return nil }
func (c *ctrlSink) SetVolume(float64) error   { return nil }
func (c *ctrlSink) Close() error              { return nil }
func (c *ctrlSink) Events() <-chan sink.Event { return c.events }

func (c *ctrlSink) Open(_ context.Context, p sink.StreamPlan) error {
	c.mu.Lock()
	c.opened = append(c.opened, p)
	c.mu.Unlock()
	return nil
}

func (c *ctrlSink) Start(time.Time) error {
	c.mu.Lock()
	c.starts++
	c.mu.Unlock()
	return nil
}

func (c *ctrlSink) Stop() error {
	c.mu.Lock()
	c.stops++
	c.mu.Unlock()
	return nil
}

func (c *ctrlSink) Pause() error {
	c.mu.Lock()
	c.pauses++
	c.mu.Unlock()
	return nil
}

// PlaybackPosition 实现 sink.PositionReporter。
func (c *ctrlSink) PlaybackPosition() (time.Duration, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pos, c.hasPos
}

func (c *ctrlSink) setPos(d time.Duration) {
	c.mu.Lock()
	c.pos = d
	c.hasPos = true
	c.mu.Unlock()
}

func (c *ctrlSink) snapshot() (opens int, starts int, stops int, pauses int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.opened), c.starts, c.stops, c.pauses
}

func (c *ctrlSink) lastPlan() sink.StreamPlan {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.opened) == 0 {
		return sink.StreamPlan{}
	}
	return c.opened[len(c.opened)-1]
}

// staticResolver 固定曲目表。
type staticResolver map[int64]Track

func (r staticResolver) ResolveTrack(_ context.Context, id int64) (Track, error) {
	return r[id], nil
}

func newDelegatedEngine(t *testing.T, tracks staticResolver) *Engine {
	t.Helper()
	return NewEngine("group", "g1", EngineDeps{
		Logger:   slog.Default(),
		Resolver: tracks,
	})
}

// waitState 等到引擎进入目标状态。
func waitState(t *testing.T, e *Engine, want State) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.Status().State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state = %s, want %s (err=%q)", e.Status().State, want, e.Status().Error)
}

// TestDelegatedPlaybackWithoutPushSinks 纯控制型组必须能正常起播。
//
// 回归：以前引擎无条件跑解码环，组内只有 local sink 时分发器拿到 0 个目标，
// 直接以 "distributor: no sinks" 把状态机打成 error——而指令其实已经生效，
// 客户端正在正常出声，用户看到的是「报错但还在响」。
func TestDelegatedPlaybackWithoutPushSinks(t *testing.T) {
	cs := newCtrlSink("web")
	e := newDelegatedEngine(t, staticResolver{
		1: {SongID: 1, Title: "a", Duration: 10 * time.Second},
	})
	e.SetControls([]sink.Sink{cs})

	if err := e.Load(context.Background(), []int64{1}, 0, 0); err != nil {
		t.Fatalf("Load: %v", err)
	}
	waitState(t, e, StatePlaying)

	if got := e.Status().Error; got != "" {
		t.Fatalf("state=playing 时不应有错误，得到 %q", got)
	}
	opens, starts, _, _ := cs.snapshot()
	if opens != 1 || starts != 1 {
		t.Fatalf("指令下发次数 open=%d start=%d, want 1/1", opens, starts)
	}
	if plan := cs.lastPlan(); plan.SongID != 1 {
		t.Fatalf("load 指令 song_id=%d, want 1", plan.SongID)
	}
}

// TestDelegatedFollowsReportedPosition 位置以客户端上报为准。
func TestDelegatedFollowsReportedPosition(t *testing.T) {
	cs := newCtrlSink("web")
	e := newDelegatedEngine(t, staticResolver{
		1: {SongID: 1, Title: "a", Duration: 30 * time.Second},
	})
	e.SetControls([]sink.Sink{cs})

	if err := e.Load(context.Background(), []int64{1}, 0, 0); err != nil {
		t.Fatalf("Load: %v", err)
	}
	waitState(t, e, StatePlaying)

	cs.setPos(7 * time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if pos := e.Status().Position; pos >= 6.9 && pos < 8.5 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("position = %.2f, want ≈7.0", e.Status().Position)
}

// TestDelegatedAdvancesToNextSong 客户端报播完后自动切下一首并重新下发指令。
func TestDelegatedAdvancesToNextSong(t *testing.T) {
	cs := newCtrlSink("web")
	e := newDelegatedEngine(t, staticResolver{
		1: {SongID: 1, Title: "a", Duration: 200 * time.Millisecond},
		2: {SongID: 2, Title: "b", Duration: 30 * time.Second},
	})
	e.SetControls([]sink.Sink{cs})

	if err := e.Load(context.Background(), []int64{1, 2}, 0, 0); err != nil {
		t.Fatalf("Load: %v", err)
	}
	waitState(t, e, StatePlaying)

	cs.setPos(300 * time.Millisecond)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if opens, _, _, _ := cs.snapshot(); opens >= 2 {
			if plan := cs.lastPlan(); plan.SongID != 2 {
				t.Fatalf("第二首 load song_id=%d, want 2", plan.SongID)
			}
			if got := e.Status().SongID; got != 2 {
				t.Fatalf("当前曲目 = %d, want 2", got)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	opens, _, _, _ := cs.snapshot()
	t.Fatalf("没有切到下一首，open 次数 = %d", opens)
}

// TestDelegatedPauseSendsCommand 暂停必须显式下指令，否则客户端会继续出声。
func TestDelegatedPauseSendsCommand(t *testing.T) {
	cs := newCtrlSink("web")
	e := newDelegatedEngine(t, staticResolver{
		1: {SongID: 1, Title: "a", Duration: 30 * time.Second},
	})
	e.SetControls([]sink.Sink{cs})

	if err := e.Load(context.Background(), []int64{1}, 0, 0); err != nil {
		t.Fatalf("Load: %v", err)
	}
	waitState(t, e, StatePlaying)

	if err := e.Pause(); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if _, _, _, pauses := cs.snapshot(); pauses != 1 {
		t.Fatalf("pause 指令次数 = %d, want 1", pauses)
	}
	if got := e.Status().State; got != StatePaused {
		t.Fatalf("state = %s, want paused", got)
	}
}
