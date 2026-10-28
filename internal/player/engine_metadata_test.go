package player_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"songloft/internal/player"
	"songloft/internal/player/pipeline"
	"songloft/internal/player/sink"
	"songloft/internal/player/sink/fake"
)

// --- 元数据推送测试替身 ---

// metaSink 在 fake.Sink 之上记录元数据 / 进度 / 封面推送。
type metaSink struct {
	*fake.Sink
	mu         sync.Mutex
	metas      []sink.NowPlaying
	progs      [][2]time.Duration
	progTracks []int
	arts       []string
}

func (s *metaSink) UpdateMetadata(m sink.NowPlaying) error {
	s.mu.Lock()
	s.metas = append(s.metas, m)
	s.mu.Unlock()
	return nil
}

func (s *metaSink) UpdateProgress(pos, total time.Duration) error {
	s.mu.Lock()
	s.progs = append(s.progs, [2]time.Duration{pos, total})
	s.progTracks = append(s.progTracks, len(s.metas)-1)
	s.mu.Unlock()
	return nil
}

func (s *metaSink) UpdateArtwork(path string) error {
	s.mu.Lock()
	s.arts = append(s.arts, path)
	s.mu.Unlock()
	return nil
}

func (s *metaSink) snapshot() (metas []sink.NowPlaying, progs [][2]time.Duration, arts []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sink.NowPlaying(nil), s.metas...),
		append([][2]time.Duration(nil), s.progs...),
		append([]string(nil), s.arts...)
}

type metaBuilder struct {
	mu    sync.Mutex
	sinks map[string]*metaSink
}

func (b *metaBuilder) Build(_ context.Context, p *player.Player) (sink.Sink, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if s, ok := b.sinks[p.ID]; ok && !s.Closed() {
		return s, nil
	}
	s := &metaSink{Sink: fake.New(p.ID)}
	b.sinks[p.ID] = s
	return s, nil
}

func (b *metaBuilder) get(id string) *metaSink {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sinks[id]
}

type mapResolver struct {
	tracks map[int64]player.Track
}

func (r mapResolver) ResolveTrack(_ context.Context, songID int64) (player.Track, error) {
	t, ok := r.tracks[songID]
	if !ok {
		return player.Track{}, fmt.Errorf("mapResolver: no track %d", songID)
	}
	return t, nil
}

// TestEnginePushesMetadata A1：起播后引擎向实现元数据接口的 sink 推送
// 进度（1s 一推、total 为曲目时长）与曲目变化时的元数据 / 封面。
func TestEnginePushesMetadata(t *testing.T) {
	builder := &metaBuilder{sinks: map[string]*metaSink{}}
	dec := &toneDecoderFactory{seconds: 3}
	resolver := mapResolver{tracks: map[int64]player.Track{
		11: {SongID: 11, Title: "歌一", Path: "/a/11.flac", Duration: 3 * time.Second,
			Artist: "艺人一", Album: "专辑一", ArtworkPath: "/covers/11.jpg"},
		12: {SongID: 12, Title: "歌二", Path: "/a/12.flac", Duration: 3 * time.Second,
			Artist: "艺人二", Album: "专辑二", ArtworkPath: "/covers/12.jpg"},
	}}
	m := player.NewManager(player.ManagerDeps{
		Store:    newMemStore(),
		Bus:      player.NewEventBus(),
		Budget:   player.NewBudget(4),
		Resolver: resolver,
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
		Clock:   time.Now,
		Latency: 200 * time.Millisecond,
	})
	ctx := context.Background()
	if err := m.RegisterPlayer(ctx, player.Player{ID: "a", Kind: player.KindRAOP, Name: "a"}); err != nil {
		t.Fatalf("register a: %v", err)
	}
	gid := m.GroupIDOf("a")
	e, err := m.Engine(gid)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	if err := e.Load(ctx, []int64{11, 12}, 0, 0); err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := e.Play(ctx); err != nil {
		t.Fatalf("play: %v", err)
	}
	a := builder.get("a")

	// 3s 内收到 ≥2 次进度推送，total 等于曲目时长，同一曲内位置递增（换曲后归零）。
	waitFor(t, "进度推送 ≥2 次", 3*time.Second, func() bool {
		_, progs, _ := a.snapshot()
		return len(progs) >= 2
	})
	_, progs, _ := a.snapshot()
	for i, p := range progs {
		if p[1] != 3*time.Second {
			t.Fatalf("第 %d 次进度 total = %v，期望 3s", i, p[1])
		}
	}
	a.mu.Lock()
	progressTracks := append([]int(nil), a.progTracks...)
	a.mu.Unlock()
	for i := 1; i < len(progs); i++ {
		if progressTracks[i] == progressTracks[i-1] && progs[i][0] < progs[i-1][0] {
			t.Fatalf("同一曲进度倒退：%v → %v", progs[i-1][0], progs[i][0])
		}
	}

	// 曲目变化时收到元数据 + 封面（首曲立即、切歌后再来一次，Title 变化）。
	waitFor(t, "切歌后再次收到元数据", 6*time.Second, func() bool {
		metas, _, arts := a.snapshot()
		return len(metas) >= 2 && len(arts) >= 2 &&
			metas[0].Title != metas[1].Title
	})
	metas, _, arts := a.snapshot()
	want := []sink.NowPlaying{
		{Title: "歌一", Artist: "艺人一", Album: "专辑一"},
		{Title: "歌二", Artist: "艺人二", Album: "专辑二"},
	}
	for i, w := range want {
		if metas[i] != w {
			t.Fatalf("第 %d 次元数据 = %+v，期望 %+v", i, metas[i], w)
		}
		if arts[i] != fmt.Sprintf("/covers/1%d.jpg", i+1) {
			t.Fatalf("第 %d 次封面路径 = %q", i, arts[i])
		}
	}
}
