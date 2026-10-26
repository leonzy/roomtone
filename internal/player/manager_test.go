package player_test

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"songloft/internal/player"
	"songloft/internal/player/pipeline"
	"songloft/internal/player/sink"
	"songloft/internal/player/sink/fake"
)

// --- 测试替身 ---

// memStore 内存版 Store：只保证读写一致，不做任何校验。
type memStore struct {
	mu      sync.Mutex
	players map[string]player.Player
	groups  map[string]player.Group
	members map[string][]player.Member
	queues  map[string][]int64
	states  map[string]player.StateSnapshot
}

func newMemStore() *memStore {
	return &memStore{
		players: map[string]player.Player{},
		groups:  map[string]player.Group{},
		members: map[string][]player.Member{},
		queues:  map[string][]int64{},
		states:  map[string]player.StateSnapshot{},
	}
}

func (s *memStore) SaveQueue(_ context.Context, kind, id string, songIDs []int64) error {
	s.mu.Lock()
	s.queues[kind+":"+id] = append([]int64(nil), songIDs...)
	s.mu.Unlock()
	return nil
}

func (s *memStore) LoadQueue(_ context.Context, kind, id string) ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.queues[kind+":"+id]...), nil
}

func (s *memStore) SaveState(_ context.Context, kind, id string, st player.StateSnapshot) error {
	s.mu.Lock()
	s.states[kind+":"+id] = st
	s.mu.Unlock()
	return nil
}

func (s *memStore) LoadState(_ context.Context, kind, id string) (player.StateSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.states[kind+":"+id], nil
}

func (s *memStore) ListPlayers(context.Context) ([]player.Player, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]player.Player, 0, len(s.players))
	for _, p := range s.players {
		out = append(out, p)
	}
	return out, nil
}

func (s *memStore) save(p player.Player) error {
	s.mu.Lock()
	s.players[p.ID] = p
	s.mu.Unlock()
	return nil
}

func (s *memStore) SavePlayer(_ context.Context, p player.Player) error         { return s.save(p) }
func (s *memStore) UpdatePlayerConfig(_ context.Context, p player.Player) error { return s.save(p) }
func (s *memStore) UpdatePlayerVolume(_ context.Context, p player.Player) error { return s.save(p) }
func (s *memStore) UpdatePlayerEQ(_ context.Context, p player.Player) error     { return s.save(p) }

func (s *memStore) DeletePlayer(_ context.Context, id string) error {
	s.mu.Lock()
	delete(s.players, id)
	s.mu.Unlock()
	return nil
}

func (s *memStore) ListGroups(context.Context) ([]player.Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]player.Group, 0, len(s.groups))
	for _, g := range s.groups {
		out = append(out, g)
	}
	return out, nil
}

func (s *memStore) SaveGroup(_ context.Context, g player.Group) error {
	s.mu.Lock()
	s.groups[g.ID] = g
	s.mu.Unlock()
	return nil
}

func (s *memStore) UpdateGroupIdentity(_ context.Context, id, name, icon string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[id]
	if !ok {
		return player.ErrGroupNotFound
	}
	g.Name = name
	g.Icon = icon
	s.groups[id] = g
	return nil
}

func (s *memStore) DeleteGroup(_ context.Context, id string) error {
	s.mu.Lock()
	delete(s.groups, id)
	s.mu.Unlock()
	return nil
}

func (s *memStore) ListAllMembers(context.Context) (map[string][]player.Member, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]player.Member, len(s.members))
	for k, v := range s.members {
		out[k] = append([]player.Member(nil), v...)
	}
	return out, nil
}

func (s *memStore) UpdateMemberPosition(_ context.Context, groupID, playerID string, position int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms := s.members[groupID]
	for i := range ms {
		if ms[i].PlayerID == playerID {
			ms[i].Position = position
		}
	}
	s.members[groupID] = ms
	return nil
}

func (s *memStore) SaveMember(_ context.Context, groupID string, m player.Member) error {
	s.mu.Lock()
	ms := s.members[groupID]
	replaced := false
	for i := range ms {
		if ms[i].PlayerID == m.PlayerID {
			ms[i] = m
			replaced = true
		}
	}
	if !replaced {
		ms = append(ms, m)
	}
	s.members[groupID] = ms
	s.mu.Unlock()
	return nil
}

func (s *memStore) DeleteMember(_ context.Context, groupID, playerID string) error {
	s.mu.Lock()
	ms := s.members[groupID]
	for i := range ms {
		if ms[i].PlayerID == playerID {
			ms = append(ms[:i], ms[i+1:]...)
			break
		}
	}
	s.members[groupID] = ms
	s.mu.Unlock()
	return nil
}

// toneDecoder 立刻吐出固定时长的非零 PCM（节奏由分发器控制）。
type toneDecoder struct {
	remain int64
}

func (d *toneDecoder) Read(p []byte) (int, error) {
	if d.remain <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > d.remain {
		n = d.remain
	}
	for i := int64(0); i < n; i++ {
		p[i] = 1 // 非零，便于和欠载补的静音帧区分
	}
	d.remain -= n
	return int(n), nil
}

func (d *toneDecoder) Close() error { return nil }
func (d *toneDecoder) Err() error   { return nil }

type toneDecoderFactory struct {
	seconds int
	mu      sync.Mutex
	starts  []time.Duration
}

func (f *toneDecoderFactory) Open(_ context.Context, _ player.Track, startAt time.Duration, _ player.Format) (player.Decoder, error) {
	f.mu.Lock()
	f.starts = append(f.starts, startAt)
	f.mu.Unlock()
	remain := int64(f.seconds)*44100*4 - int64(startAt.Seconds()*44100*4)
	if remain < 0 {
		remain = 0
	}
	return &toneDecoder{remain: remain}, nil
}

type fakeBuilder struct {
	mu    sync.Mutex
	sinks map[string]*fake.Sink
	caps  map[string]sink.Caps
}

func (b *fakeBuilder) Build(_ context.Context, p *player.Player) (sink.Sink, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// 已 Close 的 sink 不能复用：RemoveMember 会 Close 旧 sink，
	// 再 Open 就是往已关闭的 events channel 里发事件（panic）。
	if s, ok := b.sinks[p.ID]; ok && !s.Closed() {
		return s, nil
	}
	s := fake.New(p.ID)
	if c, ok := b.caps[p.ID]; ok {
		s.SetCaps(c)
	}
	b.sinks[p.ID] = s
	return s, nil
}

func (b *fakeBuilder) get(id string) *fake.Sink {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sinks[id]
}

// --- 夹具 ---

func newTestManager(t *testing.T, playerIDs ...string) (*player.Manager, *fakeBuilder, *toneDecoderFactory) {
	t.Helper()
	return newTestManagerWithClock(t, time.Now, playerIDs...)
}

// newTestManagerWithClock 与 newTestManager 相同，但可注入可控时钟。
//
// 对齐窗口（3 个校正周期）这类与时间有关的断言必须能"快进"，
// 否则测试要真等 3 秒。
func newTestManagerWithClock(t *testing.T, clock func() time.Time, playerIDs ...string) (*player.Manager, *fakeBuilder, *toneDecoderFactory) {
	t.Helper()
	return newTestManagerWithStore(t, newMemStore(), clock, playerIDs...)
}

// newTestManagerWithStore 用调用方给的 store 建管理器，便于模拟重启（同一个 store 再 Restore 一次）。
func newTestManagerWithStore(t *testing.T, store player.Store, clock func() time.Time, playerIDs ...string) (*player.Manager, *fakeBuilder, *toneDecoderFactory) {
	t.Helper()
	builder := &fakeBuilder{sinks: map[string]*fake.Sink{}, caps: map[string]sink.Caps{}}
	dec := &toneDecoderFactory{seconds: 600}
	m := player.NewManager(player.ManagerDeps{
		Store:    store,
		Bus:      player.NewEventBus(),
		Budget:   player.NewBudget(4),
		Resolver: nil,
		Decoder:  dec,
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
		Clock:   clock,
		Latency: 200 * time.Millisecond,
	})
	for _, id := range playerIDs {
		if err := m.RegisterPlayer(context.Background(), player.Player{ID: id, Kind: player.KindRAOP, Name: id}); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	return m, builder, dec
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

// TestAddMemberMidPlayback B5-01：播放中加入成员 70ms 内出声，原成员不断流。
func TestAddMemberMidPlayback(t *testing.T) {
	m, builder, _ := newTestManager(t, "a", "b")
	ctx := context.Background()
	gid := m.GroupIDOf("a")
	e, err := m.Engine(gid)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if err := e.Load(ctx, []int64{1}, 0, 0); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := e.Play(ctx); err != nil {
		t.Fatalf("play: %v", err)
	}
	a := builder.get("a")
	waitFor(t, "a 起播", 2*time.Second, func() bool { return a.FrameCount() >= 5 })
	before := a.FrameCount()

	start := time.Now()
	if _, err := m.AddMember(ctx, gid, "b"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	b := builder.get("b")
	waitFor(t, "b 收到音频", 500*time.Millisecond, func() bool { return b.FrameCount() >= 1 })
	if elapsed := time.Since(start); elapsed > 70*time.Millisecond {
		t.Fatalf("新成员 %v 才出声，超过 70ms", elapsed)
	}
	waitFor(t, "a 继续推进", time.Second, func() bool { return a.FrameCount() > before+2 })
	if got := a.SilentFrames(); got > 3 {
		t.Fatalf("原成员出现 %d 帧欠载静音", got)
	}
	if first := b.Frames()[0]; first.SampleIndex <= 0 {
		t.Fatalf("新成员应从当前进度接入，首帧序号却是 %d", first.SampleIndex)
	}
}

// TestMemberDelayB04 B5-04：delay_ms 换算成起播偏移，误差 ≤1 帧（20ms）。
func TestMemberDelayB04(t *testing.T) {
	m, builder, _ := newTestManager(t, "a", "b")
	ctx := context.Background()
	gid := m.GroupIDOf("a")
	if _, err := m.AddMember(ctx, gid, "b"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := m.UpdateMember(ctx, gid, "b", 60, 0); err != nil {
		t.Fatalf("update member: %v", err)
	}
	e, _ := m.Engine(gid)
	if err := e.Load(ctx, []int64{1}, 0, 0); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := e.Play(ctx); err != nil {
		t.Fatalf("play: %v", err)
	}
	a, b := builder.get("a"), builder.get("b")
	waitFor(t, "两台都起播", 2*time.Second, func() bool {
		return !a.StartedAt().IsZero() && !b.StartedAt().IsZero()
	})
	diff := b.StartedAt().Sub(a.StartedAt())
	if diff < 40*time.Millisecond || diff > 80*time.Millisecond {
		t.Fatalf("delay_ms=60 的起播偏移是 %v，误差超过 1 帧", diff)
	}
}

// TestTransferCompensationB05 B5-05：转移后目标首帧位置 = 源位置 + 800ms，且不叠加。
func TestTransferCompensationB05(t *testing.T) {
	m, builder, _ := newTestManager(t, "a", "b")
	ctx := context.Background()
	from := m.GroupIDOf("a")
	to := m.GroupIDOf("b")

	e, _ := m.Engine(from)
	if err := e.Load(ctx, []int64{1}, 0, 0); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := e.Play(ctx); err != nil {
		t.Fatalf("play: %v", err)
	}
	waitFor(t, "源组推进", 2*time.Second, func() bool { return builder.get("a").FrameCount() >= 10 })

	posBefore := time.Duration(e.Status().Position * float64(time.Second))
	if err := m.Transfer(ctx, from, to, ""); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	waitFor(t, "目标组挂载", 2*time.Second, func() bool { return builder.get("b").Opened() })
	got := builder.get("b").Plan().Position
	if delta := got - posBefore; delta < 780*time.Millisecond || delta > 820*time.Millisecond {
		t.Fatalf("转移补偿应在 [780,820]ms，实际 %v（源 %v → 目标 %v）", delta, posBefore, got)
	}

	waitFor(t, "目标组推进", 2*time.Second, func() bool { return builder.get("b").FrameCount() >= 5 })
	toEngine, _ := m.Engine(to)
	posBefore = time.Duration(toEngine.Status().Position * float64(time.Second))
	if err := m.Transfer(ctx, to, from, ""); err != nil {
		t.Fatalf("transfer back: %v", err)
	}
	waitFor(t, "回切挂载", 2*time.Second, func() bool { return builder.get("a").Plan().Position > 0 })
	got = builder.get("a").Plan().Position
	if delta := got - posBefore; delta < 780*time.Millisecond || delta > 820*time.Millisecond {
		t.Fatalf("第二次转移补偿仍应是 800ms，实际 %v", delta)
	}
}

// TestAddMemberRejectsDLNAInExactGroup B5-02：精确同步组拒绝只能对齐起播的设备。
func TestAddMemberRejectsDLNAInExactGroup(t *testing.T) {
	m, _, _ := newTestManager(t, "a")
	ctx := context.Background()
	if err := m.RegisterPlayer(ctx, player.Player{ID: "d", Kind: player.KindDLNA, Name: "客厅电视",
		Caps: player.Caps{Codecs: []player.Codec{player.CodecWAV}, MaxSampleRate: 48000, Channels: 2, BitDepth: 16, SyncTier: player.SyncAligned}}); err != nil {
		t.Fatalf("register: %v", err)
	}
	gid := m.GroupIDOf("a")
	if _, err := m.AddMember(ctx, gid, "d"); err == nil {
		t.Fatal("DLNA 不该能加入精确同步组")
	} else if !errors.Is(err, player.ErrSyncTierMismatch) {
		t.Fatalf("错误类型不对：%v", err)
	}
	if view, _ := m.GetGroup(gid); view.SyncTier != "exact" {
		t.Fatalf("纯 RAOP 组应恒为 exact，实际 %s", view.SyncTier)
	}
}

// TestSetVolumeClampsToMaxVolume max_volume 是硬上限：SetVolume 必须钳位。
func TestSetVolumeClampsToMaxVolume(t *testing.T) {
	m, _, _ := newTestManager(t, "a")
	ctx := context.Background()
	maxVolume := 60
	if _, err := m.UpdatePlayer(ctx, "a", player.PlayerUpdate{MaxVolume: &maxVolume}); err != nil {
		t.Fatalf("update max volume: %v", err)
	}
	p, err := m.SetVolume(ctx, "a", 80, false)
	if err != nil {
		t.Fatalf("set volume: %v", err)
	}
	if p.Volume != 60 {
		t.Fatalf("音量 = %d，期望钳到 60", p.Volume)
	}
	got, err := m.GetPlayer("a")
	if err != nil {
		t.Fatalf("get player: %v", err)
	}
	if got.Volume != 60 || got.MaxVolume != 60 {
		t.Fatalf("落库值 = volume %d / max %d，期望 60 / 60", got.Volume, got.MaxVolume)
	}
}

// TestSetVolumeFixedModeIgnores volume_mode=fixed 时忽略 SetVolume，返回当前值且不报错。
func TestSetVolumeFixedModeIgnores(t *testing.T) {
	m, _, _ := newTestManager(t, "a")
	ctx := context.Background()
	if _, err := m.SetVolume(ctx, "a", 40, false); err != nil {
		t.Fatalf("set initial volume: %v", err)
	}
	mode := player.VolumeModeFixed
	if _, err := m.UpdatePlayer(ctx, "a", player.PlayerUpdate{VolumeMode: &mode}); err != nil {
		t.Fatalf("update volume mode: %v", err)
	}
	p, err := m.SetVolume(ctx, "a", 80, true)
	if err != nil {
		t.Fatalf("fixed 模式不该报错：%v", err)
	}
	if p.Volume != 40 || p.Muted {
		t.Fatalf("fixed 模式应保持当前值 40/未静音，实际 %d/%v", p.Volume, p.Muted)
	}
	got, err := m.GetPlayer("a")
	if err != nil {
		t.Fatalf("get player: %v", err)
	}
	if got.Volume != 40 || got.Muted {
		t.Fatalf("内存值被改动：%d/%v", got.Volume, got.Muted)
	}
}

// TestUpdatePlayerMaxVolumeClampsCurrent 调低上限时当前音量一并钳位。
func TestUpdatePlayerMaxVolumeClampsCurrent(t *testing.T) {
	m, _, _ := newTestManager(t, "a")
	ctx := context.Background()
	if _, err := m.SetVolume(ctx, "a", 80, false); err != nil {
		t.Fatalf("set volume: %v", err)
	}
	maxVolume := 60
	p, err := m.UpdatePlayer(ctx, "a", player.PlayerUpdate{MaxVolume: &maxVolume})
	if err != nil {
		t.Fatalf("update max volume: %v", err)
	}
	if p.Volume != 60 {
		t.Fatalf("当前音量 = %d，期望随上限钳到 60", p.Volume)
	}
}

// TestUpdatePlayerRejectsInvalidVolumeSettings 非法模式 / 越界上限返回参数错误。
func TestUpdatePlayerRejectsInvalidVolumeSettings(t *testing.T) {
	m, _, _ := newTestManager(t, "a")
	ctx := context.Background()
	badMode := "hardware"
	if _, err := m.UpdatePlayer(ctx, "a", player.PlayerUpdate{VolumeMode: &badMode}); !errors.Is(err, player.ErrInvalidArgument) {
		t.Fatalf("非法 volume_mode 错误 = %v，期望 ErrInvalidArgument", err)
	}
	badMax := 101
	if _, err := m.UpdatePlayer(ctx, "a", player.PlayerUpdate{MaxVolume: &badMax}); !errors.Is(err, player.ErrInvalidArgument) {
		t.Fatalf("越界 max_volume 错误 = %v，期望 ErrInvalidArgument", err)
	}
}
