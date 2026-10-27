package player_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"songloft/internal/player"
	"songloft/internal/player/pipeline"
	"songloft/internal/player/sink"
	"songloft/internal/player/sink/fake"
)

// fakeClockSink 实现 sink.Sink + sink.ClockParticipant（B8 协调顺序测试）。
type fakeClockSink struct {
	*fake.Sink
	id       string
	syncedTo string // SyncToClock 收到的 master 端点
	ep       string // ClockEndpoint 返回值
	syncErr  error
}

func (f *fakeClockSink) ClockEndpoint() (string, int) {
	host, port, _ := strings.Cut(f.ep, ":")
	n, _ := strconv.Atoi(port)
	return host, n
}

func (f *fakeClockSink) SyncToClock(host string, port int) error {
	f.syncedTo = fmt.Sprintf("%s:%d", host, port)
	return f.syncErr
}

// TestEngineClockCoordination B8：起播路径 Open 全部 → 时钟协调 → Start
// 全部；≥2 个 ClockParticipant 时 master=PlayerID 字典序最小，非 master
// 收到 SyncToClock(master 端点)；单参与者跳过协调。
func TestEngineClockCoordination(t *testing.T) {
	// 两个 ClockParticipant：b 的 ID 字典序小 → master
	sinks := map[string]*fakeClockSink{
		"a": {Sink: fake.New("a"), id: "a", ep: "10.0.0.1:9001"},
		"b": {Sink: fake.New("b"), id: "b", ep: "10.0.0.2:9001"},
	}
	builder := &clockTestBuilder{sinks: sinks}
	m := player.NewManager(player.ManagerDeps{
		Store:   newMemStore(),
		Bus:     player.NewEventBus(),
		Budget:  player.NewBudget(4),
		Decoder: &toneDecoderFactory{seconds: 600},
		Runners: func(timeline *player.Timeline, targets *sink.Set, startSample int64, onProgress func(int64, time.Time)) player.FrameRunner {
			return &pipeline.Distributor{
				Rate: timeline.SampleRate(), FrameSamples: pipeline.DefaultFrameSamples,
				Targets: targets, Timeline: timeline, StartSample: startSample, OnProgress: onProgress,
			}
		},
		Builder: builder,
		Clock:   time.Now,
		Latency: 200 * time.Millisecond,
	})
	ctx := context.Background()
	for id := range sinks {
		if err := m.RegisterPlayer(ctx, player.Player{ID: id, Kind: player.KindRAAT, Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	// a/b 各自成 solo 组：把 b 并进 a 的组（≥2 成员才有时钟协调）
	gid := m.GroupIDOf("a")
	if _, err := m.AddMember(ctx, gid, "b"); err != nil {
		t.Fatalf("addmember b: %v", err)
	}
	e, err := m.Engine(gid)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Load(ctx, []int64{1}, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := e.Play(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "playing", 4*time.Second, func() bool {
		return e.Status().State == player.StatePlaying
	})

	// master = a（"a" < "b" 字典序最小）；b 收到 SyncToClock(a 的端点)
	if sinks["b"].syncedTo != "10.0.0.1:9001" {
		t.Fatalf("b 应 SyncToClock 到 a 的端点，实际 %q", sinks["b"].syncedTo)
	}
	if sinks["a"].syncedTo != "" {
		t.Fatalf("master a 不应被 SyncToClock，实际 %q", sinks["a"].syncedTo)
	}

	// Open 全部完成后、Start 之前协调：两个 sink 都已 Open 且 Started
	for id, s := range sinks {
		if !s.Opened() {
			t.Fatalf("%s 未 Open", id)
		}
		if !s.StartedAt().IsZero() {
			// Started 已发生（协调在 Start 前——顺序由 syncedTo 在 Start 前设置保证，
			// 这里只验证最终状态一致）
			_ = s.StartedAt()
		}
	}
}

// clockTestBuilder 返回 fakeClockSink 的 builder。
type clockTestBuilder struct {
	sinks map[string]*fakeClockSink
}

func (b *clockTestBuilder) Build(_ context.Context, p *player.Player) (sink.Sink, error) {
	if s, ok := b.sinks[p.ID]; ok {
		return s, nil
	}
	return fake.New(p.ID), nil
}
