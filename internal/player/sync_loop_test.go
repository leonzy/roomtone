package player

import (
	"sync"
	"testing"
	"time"

	"songloft/internal/player/sink"
	"songloft/internal/player/sink/fake"
)

// posSink 位置可控的假设备：PlaybackPosition 返回 pos，Skip/Pad 由 fakeCorrector 改它。
type posSink struct {
	*fake.Sink
	mu     sync.Mutex
	pos    time.Duration
	report bool
}

func newPosSink(id string, pos time.Duration) *posSink {
	return &posSink{Sink: fake.New(id), pos: pos, report: true}
}

func (p *posSink) PlaybackPosition() (time.Duration, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pos, p.report
}

func (p *posSink) shift(d time.Duration) {
	p.mu.Lock()
	p.pos += d
	p.mu.Unlock()
}

func (p *posSink) position() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pos
}

// fakeCorrector 把校正量直接作用到假设备的位置上，模拟真实分发器的效果。
type fakeCorrector struct {
	mu    sync.Mutex
	sinks map[string]*posSink
	calls []SyncAdjustment
}

func (c *fakeCorrector) Skip(id string, d time.Duration) error {
	c.mu.Lock()
	c.calls = append(c.calls, SyncAdjustment{PlayerID: id, Action: SyncSkip, Applied: d})
	s := c.sinks[id]
	c.mu.Unlock()
	s.shift(d) // 丢帧 = 内容往前跳
	return nil
}

func (c *fakeCorrector) Pad(id string, d time.Duration) error {
	c.mu.Lock()
	c.calls = append(c.calls, SyncAdjustment{PlayerID: id, Action: SyncPad, Applied: d})
	s := c.sinks[id]
	c.mu.Unlock()
	s.shift(-d) // 插静音 = 内容往后挪
	return nil
}

func (c *fakeCorrector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

func newSyncFixture(t *testing.T, positions ...time.Duration) (*SyncLoop, *fakeCorrector, []*posSink) {
	t.Helper()
	c := &fakeCorrector{sinks: map[string]*posSink{}}
	sinks := make([]*posSink, 0, len(positions))
	targets := make([]sink.Target, 0, len(positions))
	for i, p := range positions {
		id := string(rune('a' + i))
		s := newPosSink(id, p)
		sinks = append(sinks, s)
		c.sinks[id] = s
		targets = append(targets, sink.Target{PlayerID: id, Sink: s})
	}
	set := sink.NewSet(targets...)
	return &SyncLoop{Targets: set, Timeline: NewTimeline(time.Now(), 44100, 0), Corrector: c}, c, sinks
}

// TestSyncLoopConverges 假时钟注入 ±50ms / ±2s，1 个周期内收敛到 ≤10ms。
func TestSyncLoopConverges(t *testing.T) {
	for _, drift := range []time.Duration{50 * time.Millisecond, -50 * time.Millisecond, 2 * time.Second, -2 * time.Second} {
		loop, _, sinks := newSyncFixture(t, 10*time.Second, 10*time.Second+drift, 10*time.Second)
		loop.CheckOnce()
		// 校正的锚点是最落后的成员，因此比的是组内一致性而不是绝对位置。
		min, max := sinks[0].position(), sinks[0].position()
		for _, s := range sinks {
			if p := s.position(); p < min {
				min = p
			} else if p > max {
				max = p
			}
		}
		if d := max - min; d > DefaultMinSyncAdjust {
			t.Fatalf("drift=%v 一个周期后组内仍相差 %v", drift, d)
		}
	}
}

// TestSyncLoopIgnoresSmallDrift 偏差在容差内不动，避免每轮微调。
func TestSyncLoopIgnoresSmallDrift(t *testing.T) {
	loop, c, _ := newSyncFixture(t, 10*time.Second, 10*time.Second+5*time.Millisecond)
	loop.CheckOnce()
	if c.count() != 0 {
		t.Fatalf("5ms 偏差不该校正，实际下发 %d 次", c.count())
	}
}

// TestSyncLoopReferenceIsSlowest 参考设备取最落后的成员，只让别人等它。
func TestSyncLoopReferenceIsSlowest(t *testing.T) {
	loop, c, _ := newSyncFixture(t, 10*time.Second, 10*time.Second+80*time.Millisecond, 10*time.Second-120*time.Millisecond)
	loop.CheckOnce()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) != 2 {
		t.Fatalf("应有 2 台被校正，实际 %d", len(c.calls))
	}
	for _, call := range c.calls {
		if call.PlayerID == "c" {
			t.Fatal("最落后的 c 不该被校正（它就是参考）")
		}
	}
	// a 落后 c 120ms 相对超前 → 应插静音 120ms；b 同理 200ms。
	byID := map[string]SyncAdjustment{}
	for _, call := range c.calls {
		byID[call.PlayerID] = call
	}
	if byID["a"].Action != SyncPad || byID["a"].Applied != 120*time.Millisecond {
		t.Fatalf("a 应 Pad 120ms，实际 %+v", byID["a"])
	}
	if byID["b"].Action != SyncPad || byID["b"].Applied != 200*time.Millisecond {
		t.Fatalf("b 应 Pad 200ms，实际 %+v", byID["b"])
	}
}

// TestSyncLoopDelayCompensation delay_ms 大的设备本来就该落后，不算漂移。
func TestSyncLoopDelayCompensation(t *testing.T) {
	loop, c, _ := newSyncFixture(t, 10*time.Second, 10*time.Second)
	// b 配了 100ms 延迟补偿，它比 a 落后 100ms 是正确的。
	loop.Targets.Add(sink.Target{PlayerID: "b", Sink: loop.mustSink("b"), Delay: 100 * time.Millisecond})
	c.sinks["b"].shift(-100 * time.Millisecond)
	loop.CheckOnce()
	if c.count() != 0 {
		t.Fatalf("延迟补偿被当成漂移了：%+v", c.calls)
	}
}

func (l *SyncLoop) mustSink(id string) sink.PushSink {
	t, _ := l.Targets.Get(id)
	return t.Sink
}

// TestSyncLoopGivesUpOnRunaway 偏差 > 10s 不再硬拽，转 reattach。
func TestSyncLoopGivesUpOnRunaway(t *testing.T) {
	loop, c, _ := newSyncFixture(t, 10*time.Second, 40*time.Second)
	var got []string
	loop.OnDeviation = func(id string, d time.Duration) { got = append(got, id) }
	out := loop.CheckOnce()
	if c.count() != 0 {
		t.Fatal("跑飞的设备不该被丢帧硬拽")
	}
	if len(got) != 1 || got[0] != "b" {
		t.Fatalf("应上报 b 跑飞，实际 %v", got)
	}
	if out[1].Action != SyncResync {
		t.Fatalf("b 的判定应为 resync，实际 %s", out[1].Action)
	}
}

// TestSyncLoopLongRunResidual 10 分钟仿真：每轮漂 +2ms，残余必须 ≤5ms。
func TestSyncLoopLongRunResidual(t *testing.T) {
	loop, _, sinks := newSyncFixture(t, time.Duration(0), time.Duration(0))
	const rounds = 632 // 950ms × 632 ≈ 10 分钟
	for i := 0; i < rounds; i++ {
		sinks[1].shift(2 * time.Millisecond) // b 的晶振偏快
		loop.CheckOnce()
	}
	if d := sinks[1].position() - sinks[0].position(); d > 5*time.Millisecond || d < -5*time.Millisecond {
		t.Fatalf("10 分钟后残余漂移 %v 超过 5ms", d)
	}
}

// TestSyncLoopSkipsNonReporters 没有回传通道的设备不参与校正。
func TestSyncLoopSkipsNonReporters(t *testing.T) {
	a := newPosSink("a", 10*time.Second)
	b := newPosSink("b", 10*time.Second)
	b.report = false
	c := &fakeCorrector{sinks: map[string]*posSink{"a": a, "b": b}}
	loop := &SyncLoop{
		Targets:   sink.NewSet(sink.Target{PlayerID: "a", Sink: a}, sink.Target{PlayerID: "b", Sink: b}),
		Timeline:  NewTimeline(time.Now(), 44100, 0),
		Corrector: c,
	}
	if out := loop.CheckOnce(); len(out) != 0 {
		t.Fatalf("只有 1 台可测设备时不该校正，实际 %+v", out)
	}
}
