package player_test

import (
	"context"
	"testing"
	"time"

	"songloft/internal/player"
	"songloft/internal/player/pipeline"
	"songloft/internal/player/sink"
)

func TestGaplessNextTrackResetsPositionAndFinishesQueue(t *testing.T) {
	manager := player.NewManager(player.ManagerDeps{
		Store: newMemStore(), Budget: player.NewBudget(4), Bus: player.NewEventBus(),
		Decoder: &toneDecoderFactory{seconds: 1},
		Resolver: mapResolver{tracks: map[int64]player.Track{
			11: {SongID: 11, Duration: time.Second}, 12: {SongID: 12, Duration: time.Second},
		}},
		Builder: &metaBuilder{sinks: map[string]*metaSink{}},
		Clock:   time.Now, Latency: 100 * time.Millisecond,
		Runners: func(timeline *player.Timeline, targets *sink.Set, start int64, progress func(int64, time.Time)) player.FrameRunner {
			return &pipeline.Distributor{Rate: timeline.SampleRate(), FrameSamples: pipeline.DefaultFrameSamples,
				Targets: targets, Timeline: timeline, StartSample: start, OnProgress: progress}
		},
	})
	ctx := context.Background()
	if err := manager.RegisterPlayer(ctx, player.Player{ID: "test-output", Kind: player.KindRAOP}); err != nil {
		t.Fatal(err)
	}
	engine, err := manager.Engine(manager.GroupIDOf("test-output"))
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Stop()
	if err := engine.Load(ctx, []int64{11, 12}, 0, 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "second track", 4*time.Second, func() bool {
		status := engine.Status()
		return status.SongID == 12 && status.State == player.StatePlaying
	})
	if position := engine.Status().Position; position >= 0.5 {
		t.Fatalf("second track inherited previous position: %.3fs", position)
	}
	waitFor(t, "queue completion", 4*time.Second, func() bool { return engine.Status().State == player.StateIdle })
	if status := engine.Status(); len(status.Queue.Items) != 0 || status.SongID != 0 {
		t.Fatalf("completed order queue remains pending: %+v", status)
	}
}
