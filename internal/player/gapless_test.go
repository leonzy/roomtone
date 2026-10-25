package player_test

import (
	"context"
	"testing"
	"time"

	"songloft/internal/player"
	"songloft/internal/player/pipeline"
	"songloft/internal/player/sink"
	"songloft/internal/player/sink/fake"
)

// newShortSongManager 造一个"每首歌只有 seconds 秒"的管理器，用于跨曲用例。
func newShortSongManager(t *testing.T, seconds int, ids ...string) (*player.Manager, *fakeBuilder, *toneDecoderFactory) {
	t.Helper()
	store := newMemStore()
	builder := &fakeBuilder{sinks: map[string]*fake.Sink{}, caps: map[string]sink.Caps{}}
	dec := &toneDecoderFactory{seconds: seconds}
	m := player.NewManager(player.ManagerDeps{
		Store:   store,
		Bus:     player.NewEventBus(),
		Budget:  player.NewBudget(4),
		Decoder: dec,
		Runners: func(timeline *player.Timeline, targets *sink.Set, startSample int64, onProgress func(int64, time.Time)) player.FrameRunner {
			return &pipeline.Distributor{
				Rate:         timeline.SampleRate(),
				FrameSamples: pipeline.DefaultFrameSamples,
				Targets:      targets,
				Timeline:     timeline,
				StartSample:  startSample,
				OnProgress:   onProgress,
			}
		},
		Builder: builder,
		Clock:   time.Now,
		Latency: 200 * time.Millisecond,
	})
	for _, id := range ids {
		if err := m.RegisterPlayer(context.Background(), player.Player{ID: id, Kind: player.KindRAOP, Name: id}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	return m, builder, dec
}

// TestGaplessContinuityB0511 跨曲样本序号连续、走接续路径、无欠载静音。
//
// 断言的是"交界处"三件事：
//  1. 帧的 SampleIndex 严格递增且步长恒为 882（20ms），没有归零重来；
//  2. 走了 GaplessSink.Continue（没有重新 Open/Start）；
//  3. 交界处没有补静音（补了就是听得见的断点）。
func TestGaplessContinuityB0511(t *testing.T) {
	m, builder, dec := newShortSongManager(t, 1, "a")
	ctx := context.Background()
	gid := m.GroupIDOf("a")
	e, err := m.Engine(gid)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	// 两首歌各 1 秒：200ms 延迟下，预取窗口 3s 一开局就该命中。
	if err := e.Load(ctx, []int64{11, 12}, 0, 0); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := e.Play(ctx); err != nil {
		t.Fatalf("play: %v", err)
	}
	a := builder.get("a")
	// 等第二首的帧真的落到 sink 上（接续后的帧）。
	waitFor(t, "跨曲接续", 4*time.Second, func() bool { return len(a.Continues()) >= 1 })

	frames := a.Frames()
	if len(frames) < 10 {
		t.Fatalf("帧数太少：%d", len(frames))
	}
	for i := 1; i < len(frames); i++ {
		step := frames[i].SampleIndex - frames[i-1].SampleIndex
		if step != player.DefaultFrameSamples {
			t.Fatalf("第 %d 帧样本序号跳变 %d（%d → %d）",
				i, step, frames[i-1].SampleIndex, frames[i].SampleIndex)
		}
	}
	if got := a.SilentFrames(); got > 0 {
		t.Fatalf("跨曲处补了 %d 帧静音（听得见的断点）", got)
	}
	if cont := a.Continues(); len(cont) == 0 || cont[0] != 12 {
		t.Fatalf("接续的曲目 = %v，期望 [12]", cont)
	}
	// 两首歌各开一次解码器；预取不应重复开。
	dec.mu.Lock()
	opens := len(dec.starts)
	dec.mu.Unlock()
	if opens != 2 {
		t.Fatalf("解码器开了 %d 次，期望 2（预取命中就不该再开）", opens)
	}
}

// TestPeekNextB0511 预取只看不动游标：连续两次 PeekNext 结果一致。
func TestPeekNextB0511(t *testing.T) {
	q := player.NewQueue()
	q.Replace([]int64{1, 2, 3}, 0)
	first, ok := q.PeekNext()
	if !ok || first != 2 {
		t.Fatalf("PeekNext = %d,%v", first, ok)
	}
	if again, _ := q.PeekNext(); again != 2 {
		t.Fatalf("PeekNext 改了游标：%d", again)
	}
	if cur, _ := q.CurrentSong(); cur != 1 {
		t.Fatalf("当前曲目被改了：%d", cur)
	}
	// 队尾且不循环：没有下一首。
	q.Replace([]int64{7}, 0)
	if _, ok := q.PeekNext(); ok {
		t.Fatal("单曲不循环时不该有下一首")
	}
	// 单曲循环：下一首还是自己，调用方据此跳过预取。
	q.SetRepeat(player.RepeatOne)
	if next, ok := q.PeekNext(); ok && next != 7 {
		t.Fatalf("单曲循环的下一首 = %d", next)
	}
}
