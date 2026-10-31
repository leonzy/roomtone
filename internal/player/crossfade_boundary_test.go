package player_test

import (
	"context"
	"io"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"songloft/internal/player"
	"songloft/internal/player/pipeline"
	"songloft/internal/player/sink"
)

type boundaryRun struct {
	status player.Status
	bytes  int64
}

type boundaryRunner struct {
	engine   func() *player.Engine
	progress func(int64, time.Time)
	start    int64
	done     chan<- boundaryRun
}

func (r *boundaryRunner) Run(ctx context.Context, src io.Reader) (int, error) {
	r.progress(r.start, time.Now().Add(2*time.Second))
	status := r.engine().Status()
	n, err := io.Copy(io.Discard, src)
	r.done <- boundaryRun{status: status, bytes: n}
	return int(n / (player.DefaultFrameSamples * 4)), err
}

func TestCrossfadeAdvancesAtCurrentTrackEndAndResumesOnlyConsumedPrefix(t *testing.T) {
	verifyCrossfadeBoundary(t, &toneDecoderFactory{seconds: 4}, map[int64]player.Track{
		11: {SongID: 11, Duration: 4 * time.Second},
		12: {SongID: 12, Duration: 4 * time.Second},
		13: {SongID: 13, Duration: 4 * time.Second},
	})
}

func TestCrossfadeBoundaryWithRealFFmpeg(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg required for actual decoder integration")
	}
	source := filepath.Join(t.TempDir(), "four-seconds.wav")
	if out, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=44100:duration=4", "-ac", "2", source).CombinedOutput(); err != nil {
		t.Fatalf("generate PCM fixture: %v: %s", err, out)
	}
	verifyCrossfadeBoundary(t, pipeline.NewFFmpegDecoderFactory(func() string { return ffmpeg }, nil), map[int64]player.Track{
		11: {SongID: 11, Path: source, Duration: 4 * time.Second},
		12: {SongID: 12, Path: source, Duration: 4 * time.Second},
		13: {SongID: 13, Path: source, Duration: 4 * time.Second},
	})
}

func verifyCrossfadeBoundary(t *testing.T, decoder player.DecoderFactory, tracks map[int64]player.Track) {
	t.Helper()
	// With a three-second overlap, four-second tracks produce runs of 4, 1,
	// and 3 seconds. The second track has only one second left to mix into #3.
	done := make(chan boundaryRun, 3)
	var engine *player.Engine
	builder := &metaBuilder{sinks: map[string]*metaSink{}}
	manager := player.NewManager(player.ManagerDeps{
		Store: newMemStore(), Budget: player.NewBudget(4), Bus: player.NewEventBus(),
		Decoder:  decoder,
		Resolver: mapResolver{tracks: tracks},
		Builder:  builder,
		Clock:    time.Now, Latency: 100 * time.Millisecond,
		Crossfade: func() time.Duration { return 3 * time.Second },
		Runners: func(_ *player.Timeline, _ *sink.Set, start int64, progress func(int64, time.Time)) player.FrameRunner {
			return &boundaryRunner{engine: func() *player.Engine { return engine }, progress: progress, start: start, done: done}
		},
	})
	ctx := context.Background()
	if err := manager.RegisterPlayer(ctx, player.Player{ID: "capture", Kind: player.KindRAOP}); err != nil {
		t.Fatal(err)
	}
	var err error
	engine, err = manager.Engine(manager.GroupIDOf("capture"))
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Stop()
	if err := engine.Load(ctx, []int64{11, 12, 13}, 0, 0); err != nil {
		t.Fatal(err)
	}
	for index, want := range []struct{ seconds, position int64 }{{4, 0}, {1, 3}, {3, 1}} {
		select {
		case result := <-done:
			if result.bytes != want.seconds*44100*4 {
				t.Fatalf("track %d consumed %d PCM bytes, want %d; next track must not play under previous identity", index, result.bytes, want.seconds*44100*4)
			}
			if result.status.SongID != int64(11+index) || result.status.Queue.Current != index || result.status.Position < float64(want.position) || result.status.Position > float64(want.position)+0.1 {
				t.Fatalf("track %d identity/position incorrect: %+v", index, result.status)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("crossfade did not advance")
		}
	}
	if ids := builder.get("capture").Continues(); len(ids) != 2 || ids[0] != 12 || ids[1] != 13 {
		t.Fatalf("crossfade must retain the sink session and buffered overlap: %v", ids)
	}
}
