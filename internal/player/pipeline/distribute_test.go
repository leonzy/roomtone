package pipeline

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"songloft/internal/player"
	"songloft/internal/player/dsp"
	"songloft/internal/player/sink"
	"songloft/internal/player/sink/fake"
)

// recSink 在 fake 之上记录 PCM 原始字节，并可注入写延迟。
type recSink struct {
	*fake.Sink
	delay time.Duration
	mu    sync.Mutex
	data  [][]byte
	wrote int64
}

func newRecSink(id string) *recSink { return &recSink{Sink: fake.New(id)} }

func (r *recSink) WriteFrame(f sink.Frame) error {
	if r.delay > 0 {
		time.Sleep(r.delay)
	}
	r.mu.Lock()
	r.data = append(r.data, append([]byte(nil), f.Data...))
	r.wrote++
	r.mu.Unlock()
	return r.Sink.WriteFrame(f)
}

func (r *recSink) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.data)
}

func (r *recSink) last() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.data) == 0 {
		return nil
	}
	return r.data[len(r.data)-1]
}

// tickClock 每次调用前进 1ms：让分发器不必真的等待就能跑满速。
type tickClock struct {
	mu sync.Mutex
	n  int
	t0 time.Time
}

func newTickClock() *tickClock { return &tickClock{t0: time.Now()} }

func (c *tickClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return c.t0.Add(time.Duration(c.n) * time.Millisecond)
}

// pcmTone 生成 frames 个分发帧（每帧 FrameSamples 个采样）的 s16le 双声道 1kHz 正弦。
func pcmTone(frames int, amp float64) []byte {
	n := frames * DefaultFrameSamples
	buf := make([]byte, n*4)
	for i := 0; i < n; i++ {
		v := int16(amp * 32767 * math.Sin(2*math.Pi*1000*float64(i)/44100))
		binary.LittleEndian.PutUint16(buf[i*4:], uint16(v))
		binary.LittleEndian.PutUint16(buf[i*4+2:], uint16(v))
	}
	return buf
}

func newDistributor(clock *tickClock, set *sink.Set) *Distributor {
	return &Distributor{
		Rate:         44100,
		FrameSamples: DefaultFrameSamples,
		Targets:      set,
		Timeline:     player.NewTimeline(clock.t0, 44100, 2*time.Second),
		BufferDepth:  2 * time.Second,
		Clock:        clock.now,
		Sleep:        func(time.Duration) {},
	}
}

// TestStalledSinkDoesNotBlockOthers B5-09 验收：
// 一台设备每帧卡 500ms，另一台照常收帧，不欠载、不丢帧。
func TestStalledSinkDoesNotBlockOthers(t *testing.T) {
	const total = 120
	clock := newTickClock()
	fast := newRecSink("fast")
	stalled := newRecSink("stalled")
	stalled.delay = 500 * time.Millisecond
	set := sink.NewSet(
		sink.Target{PlayerID: "fast", Sink: fast},
		sink.Target{PlayerID: "stalled", Sink: stalled},
	)
	d := newDistributor(clock, set)

	var underruns int64
	d.OnUnderrun = func(int) { atomic.AddInt64(&underruns, 1) }

	src := bytes.NewReader(pcmTone(total, 0.2))
	done := make(chan struct{})
	go func() {
		_, _ = d.Run(context.Background(), src)
		close(done)
	}()

	// 快速那一路应当在 2s 内收齐全部帧（卡住那一路最多收几十帧）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fast.count() >= total {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := fast.count(); got < total {
		t.Fatalf("正常设备只收到 %d/%d 帧，被卡住的设备拖累了", got, total)
	}
	if got := atomic.LoadInt64(&underruns); got != 0 {
		t.Fatalf("出现 %d 次欠载：分发被阻塞", got)
	}

	// 卡住的设备队列有界，必须丢帧而不是无限堆积。
	stats := map[string]SinkStats{}
	for _, s := range d.Stats() {
		stats[s.PlayerID] = s
	}
	if stats["stalled"].Dropped == 0 && stalled.count() > 0 {
		// 允许它恰好没满（取决于调度），但队列深度不得超过上限。
		if stats["stalled"].Queued > DefaultQueueFrames+8 {
			t.Fatalf("卡住的设备队列无界增长：%d", stats["stalled"].Queued)
		}
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("分发未在预期时间内结束")
	}
}

// TestHotSwapTargets B5-09 验收：播放中加入的成员下一帧就出声，不用等下一曲。
func TestHotSwapTargets(t *testing.T) {
	const total = 400
	clock := newTickClock()
	a := newRecSink("a")
	b := newRecSink("b")
	set := sink.NewSet(sink.Target{PlayerID: "a", Sink: a})
	d := newDistributor(clock, set)

	pr, pw := io.Pipe()
	go func() {
		_, _ = d.Run(context.Background(), pr)
	}()
	go func() {
		frame := pcmTone(1, 0.2)
		for i := 0; i < total; i++ {
			if _, err := pw.Write(frame); err != nil {
				return
			}
			time.Sleep(200 * time.Microsecond)
		}
		_ = pw.Close()
	}()

	// 等 a 收到一些帧后把 b 加进来。
	for a.count() < 10 {
		time.Sleep(time.Millisecond)
	}
	if err := d.Skip("a", 0); err != nil {
		t.Fatalf("Skip 在播放中不可用：%v", err)
	}
	set.Add(sink.Target{PlayerID: "b", Sink: b})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && b.count() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if b.count() == 0 {
		t.Fatal("播放中加入的成员一帧都没收到（SetSinks 仍是开曲快照）")
	}
	// 加入后应当几乎立刻出声：a 的帧数不会领先太多。
	for time.Now().Before(deadline) && a.count() < total {
		time.Sleep(5 * time.Millisecond)
	}
	if a.count() != total {
		t.Fatalf("a 收帧不全：%d/%d", a.count(), total)
	}
	if b.count() < total-50 {
		t.Fatalf("b 加入后落后太多：%d/%d（热替换没生效）", b.count(), total)
	}
}

// TestSkipOnlyAffectsOneTarget 同步校正环：跳帧只影响被校正的那一路。
func TestSkipOnlyAffectsOneTarget(t *testing.T) {
	const total = 100
	clock := newTickClock()
	a := newRecSink("a")
	b := newRecSink("b")
	set := sink.NewSet(
		sink.Target{PlayerID: "a", Sink: a},
		sink.Target{PlayerID: "b", Sink: b},
	)
	d := newDistributor(clock, set)

	pr, pw := io.Pipe()
	go func() { _, _ = d.Run(context.Background(), pr) }()
	go func() {
		frame := pcmTone(1, 0.2)
		for i := 0; i < total; i++ {
			if _, err := pw.Write(frame); err != nil {
				return
			}
			time.Sleep(200 * time.Microsecond)
		}
		_ = pw.Close()
	}()

	for a.count() < 5 {
		time.Sleep(time.Millisecond)
	}
	// 100ms = 5 帧
	if err := d.Skip("a", 100*time.Millisecond); err != nil {
		t.Fatalf("Skip 失败：%v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (a.count() < total || b.count() < total) {
		time.Sleep(5 * time.Millisecond)
	}
	if b.count() != total {
		t.Fatalf("b 收帧数异常：%d/%d", b.count(), total)
	}
	if got := total - a.count(); got < 4 || got > 6 {
		t.Fatalf("a 应跳过约 5 帧，实际跳过 %d", got)
	}
}

// TestPerTargetEQ 每台设备独立 EQ：一台抬 1kHz，另一台必须原样。
func TestPerTargetEQ(t *testing.T) {
	const total = 40
	clock := newTickClock()
	eqd := newRecSink("eq")
	flat := newRecSink("flat")
	bands := make([]float64, dsp.BandCount)
	bands[5] = 12
	chain := dsp.NewEQ(44100, 2, bands, 0)
	set := sink.NewSet(
		sink.Target{PlayerID: "eq", Sink: eqd, EQ: chain},
		sink.Target{PlayerID: "flat", Sink: flat},
	)
	d := newDistributor(clock, set)
	if _, err := d.Run(context.Background(), bytes.NewReader(pcmTone(total, 0.1))); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if eqd.count() != total || flat.count() != total {
		t.Fatalf("收帧数不一致：eq=%d flat=%d", eqd.count(), flat.count())
	}
	// 直通那一路必须与输入逐字节相同（第 5 帧，避开起播瞬态）。
	src := pcmTone(total, 0.1)
	want := src[5*DefaultFrameSamples*4 : 6*DefaultFrameSamples*4]
	got := flat.data[5]
	if !bytes.Equal(want, got) {
		t.Fatal("没配 EQ 的目标被处理了")
	}
	if bytes.Equal(eqd.data[5], got) {
		t.Fatal("配了 EQ 的目标没有被处理（EQ 仍是组级一份）")
	}
}

// TestDistributorAppliesGain ReplayGain：SetGain 之后的帧按线性增益缩放。
func TestDistributorAppliesGain(t *testing.T) {
	clock := newTickClock()
	rec := newRecSink("a")
	set := sink.NewSet(sink.Target{PlayerID: "a", Sink: rec})
	d := newDistributor(clock, set)
	d.SetGain(0.5)

	// 幅度 0.5 → 峰值约 16383，乘 0.5 后约 8192。
	_, _ = d.Run(context.Background(), bytes.NewReader(pcmTone(2, 0.5)))
	if rec.count() == 0 {
		t.Fatal("未收到任何帧")
	}
	rec.mu.Lock()
	frame := append([]byte(nil), rec.data[0]...)
	rec.mu.Unlock()

	maxAbs := 0
	for i := 0; i+1 < len(frame); i += 2 {
		v := int(int16(binary.LittleEndian.Uint16(frame[i:])))
		if v < 0 {
			v = -v
		}
		if v > maxAbs {
			maxAbs = v
		}
	}
	if maxAbs < 7000 || maxAbs > 9000 {
		t.Fatalf("增益 0.5 后峰值应在 ~8192，实际 %d", maxAbs)
	}
}
