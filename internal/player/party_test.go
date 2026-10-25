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

// newPartyManager 三台 RAOP 设备各自一个组，派对已开启。
func newPartyManager(t *testing.T, ids ...string) (*player.Manager, *fakeBuilder) {
	t.Helper()
	builder := &fakeBuilder{sinks: map[string]*fake.Sink{}, caps: map[string]sink.Caps{}}
	dec := &toneDecoderFactory{seconds: 600}
	m := player.NewManager(player.ManagerDeps{
		Store:                 newMemStore(),
		Bus:                   player.NewEventBus(),
		Budget:                player.NewBudget(8),
		Decoder:               dec,
		PartyEnabled:          true,
		PartyDriftToleranceMs: 1500,
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
	return m, builder
}

// logicalPositions 各组当前逻辑进度（秒）。
func logicalPositions(t *testing.T, m *player.Manager, groups []string) map[string]float64 {
	t.Helper()
	out := make(map[string]float64, len(groups))
	for _, gid := range groups {
		e, err := m.Engine(gid)
		if err != nil {
			t.Fatalf("engine %s: %v", gid, err)
		}
		out[gid] = e.Status().Position
	}
	return out
}

// B7-06：3 组同播，互相逻辑进度差 < 1.5s。
func TestPartyThreeGroupsStayAligned(t *testing.T) {
	m, builder := newPartyManager(t, "a", "b", "c")
	ctx := context.Background()
	groups := []string{m.GroupIDOf("a"), m.GroupIDOf("b"), m.GroupIDOf("c")}

	p, err := m.CreateParty(ctx, "全屋", groups)
	if err != nil {
		t.Fatalf("create party: %v", err)
	}
	if err := m.PartyPlay(ctx, p.ID, []int64{1}, 0, 0); err != nil {
		t.Fatalf("party play: %v", err)
	}
	for _, id := range []string{"a", "b", "c"} {
		s := builder.get(id)
		waitFor(t, id+" 起播", 2*time.Second, func() bool { return s.FrameCount() >= 5 })
	}

	// 跑满两个采样周期，确认不会自己把自己校歪。
	time.Sleep(2 * time.Second)
	pos := logicalPositions(t, m, groups)
	min, max := 1e9, -1e9
	for _, v := range pos {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	if max-min > 1.5 {
		t.Fatalf("三组进度差 %.2fs 超过 1.5s：%v", max-min, pos)
	}
	if err := m.EndParty(ctx, p.ID); err != nil {
		t.Fatalf("end party: %v", err)
	}
}

// B7-06 + B7-07：某组被拖慢 2s，5s 内回到 1.5s 以内，并广播 corrected:true。
func TestPartyRecoversFromSlowGroup(t *testing.T) {
	m, builder := newPartyManager(t, "a", "b", "c")
	ctx := context.Background()
	ga, gb, gc := m.GroupIDOf("a"), m.GroupIDOf("b"), m.GroupIDOf("c")

	p, err := m.CreateParty(ctx, "全屋", []string{ga, gb, gc})
	if err != nil {
		t.Fatalf("create party: %v", err)
	}
	ch, cancel := m.Bus().Subscribe()
	defer cancel()
	if err := m.PartyPlay(ctx, p.ID, []int64{1}, 0, 0); err != nil {
		t.Fatalf("party play: %v", err)
	}
	for _, id := range []string{"a", "b", "c"} {
		s := builder.get(id)
		waitFor(t, id+" 起播", 2*time.Second, func() bool { return s.FrameCount() >= 5 })
	}

	// 人为制造 2s 落后：把 c 组停 2s 再继续。
	ce, err := m.Engine(gc)
	if err != nil {
		t.Fatalf("engine c: %v", err)
	}
	if err := ce.Pause(); err != nil {
		t.Fatalf("pause c: %v", err)
	}
	time.Sleep(2 * time.Second)
	if err := ce.Play(ctx); err != nil {
		t.Fatalf("resume c: %v", err)
	}

	// 5s 内必须看到 corrected:true，且 c 组回到容忍范围。
	deadline := time.After(5 * time.Second)
	corrected := false
	for !corrected {
		select {
		case e := <-ch:
			if e.Type == player.EventGroupDrift && e.GroupID == gc && e.Corrected != nil && *e.Corrected {
				corrected = true
			}
		case <-deadline:
			t.Fatalf("5s 内没有收到 corrected:true（c 组偏移 %v）", logicalPositions(t, m, []string{ga, gb, gc}))
		}
	}
	pos := logicalPositions(t, m, []string{ga, gb, gc})
	if diff := pos[ga] - pos[gc]; diff > 1.5 || diff < -1.5 {
		t.Fatalf("校正后 c 组仍偏 %.2fs：%v", diff, pos)
	}
	_ = m.EndParty(ctx, p.ID)
}

// B7-06：某组退出派对不能扰动邻组进度（< 100ms）。
func TestPartyLeaveDoesNotDisturbNeighbours(t *testing.T) {
	m, builder := newPartyManager(t, "a", "b", "c")
	ctx := context.Background()
	ga, gb, gc := m.GroupIDOf("a"), m.GroupIDOf("b"), m.GroupIDOf("c")
	p, err := m.CreateParty(ctx, "全屋", []string{ga, gb, gc})
	if err != nil {
		t.Fatalf("create party: %v", err)
	}
	if err := m.PartyPlay(ctx, p.ID, []int64{1}, 0, 0); err != nil {
		t.Fatalf("party play: %v", err)
	}
	for _, id := range []string{"a", "b", "c"} {
		s := builder.get(id)
		waitFor(t, id+" 起播", 2*time.Second, func() bool { return s.FrameCount() >= 5 })
	}
	time.Sleep(300 * time.Millisecond)

	before := logicalPositions(t, m, []string{ga, gb, gc})
	if err := m.LeaveParty(ctx, p.ID, gc, "stop"); err != nil {
		t.Fatalf("leave: %v", err)
	}
	after := logicalPositions(t, m, []string{ga, gb})
	for _, gid := range []string{ga, gb} {
		if d := after[gid] - before[gid]; d < -0.1 || d > 0.1 {
			t.Fatalf("%s 被退出事件扰动 %.3fs", gid, d)
		}
	}
	_ = m.EndParty(ctx, p.ID)
}
