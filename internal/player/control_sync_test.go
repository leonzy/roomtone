package player

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"songloft/internal/player/sink"
	"songloft/internal/player/sink/fake"
)

type aligningControl struct {
	*ctrlSink
	calls  int
	song   int64
	target time.Duration
}

func (c *aligningControl) AlignPlayback(_ context.Context, song int64, position time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.song = song
	c.target = position
	c.pos = position
	return nil
}

func TestControlSyncCorrectsPersistentPullLagAndBoundsRetries(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	member := &aligningControl{ctrlSink: newCtrlSink("dlna")}
	member.setPos(7 * time.Second)
	loop := ControlSyncLoop{Members: func() []sink.Sink { return []sink.Sink{member} }, Reference: func() (int64, time.Duration, bool) { return 2, 10 * time.Second, true }, Clock: func() time.Time { return now }}
	loop.CheckOnce(context.Background())
	if member.calls != 0 {
		t.Fatal("single quantized report triggered seek")
	}
	loop.CheckOnce(context.Background())
	if member.calls != 1 || member.song != 2 || member.target != 10*time.Second {
		t.Fatalf("persistent lag not corrected: %+v", member)
	}
	member.setPos(6 * time.Second)
	for i := 0; i < 5; i++ {
		loop.CheckOnce(context.Background())
	}
	if member.calls != 1 {
		t.Fatal("corrected again before cooldown")
	}
	now = now.Add(16 * time.Second)
	loop.CheckOnce(context.Background())
	if member.calls != 2 {
		t.Fatal("persistent drift never retried after cooldown")
	}
}

func TestControlSyncIgnoresSecondQuantizationAndTransientReversal(t *testing.T) {
	member := &aligningControl{ctrlSink: newCtrlSink("dlna")}
	loop := ControlSyncLoop{Members: func() []sink.Sink { return []sink.Sink{member} }, Reference: func() (int64, time.Duration, bool) { return 1, 10900 * time.Millisecond, true }}
	for _, position := range []time.Duration{10 * time.Second, 10 * time.Second, 8 * time.Second, 13 * time.Second, 10 * time.Second} {
		member.setPos(position)
		loop.CheckOnce(context.Background())
	}
	if member.calls != 0 {
		t.Fatal("quantized/jittery reports triggered correction")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	member.setPos(0)
	loop.CheckOnce(ctx)
	loop.CheckOnce(ctx)
	if member.calls != 0 {
		t.Fatal("canceled playback still aligned")
	}
}

func TestControlAlignmentReferenceUsesTrackPresentationNotUIClock(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	e := NewEngine("group", "mixed", EngineDeps{Clock: func() time.Time { return now }})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.running = true
	e.runCtx = ctx
	e.timeline = NewTimeline(now.Add(-100*time.Second), 44100, 0)
	e.planBase = 95 * 44100
	e.curPlan = sink.StreamPlan{SongID: 2, Position: 3 * time.Second, Duration: time.Minute}
	e.transport.Load(2, 50*time.Second)
	e.transport.MarkBuffering()
	e.transport.Play()
	song, position, valid := e.alignmentReference(ctx)
	if !valid || song != 2 || position != 8*time.Second {
		t.Fatalf("used UI/global timeline instead of current track: %d %v %v", song, position, valid)
	}
	e.delegated = true
	if _, _, valid := e.alignmentReference(ctx); valid {
		t.Fatal("solo delegated playback was aligned to nonexistent PCM")
	}
}

func TestMixedEngineRunsPullAlignment(t *testing.T) {
	var tick atomic.Int64
	initial := time.Now()
	tick.Store(initial.UnixNano())
	clock := func() time.Time { return time.Unix(0, tick.Load()) }
	member := &aligningControl{ctrlSink: newCtrlSink("dlna")}
	member.setPos(0)
	e := NewEngine("group", "mixed", EngineDeps{Clock: clock, Resolver: staticResolver{1: {SongID: 1, Duration: time.Minute}}, Decoder: completionDecoderFactory{},
		Runners: func(_ *Timeline, _ *sink.Set, start int64, progress func(int64, time.Time)) FrameRunner {
			return completionRunner{func(ctx context.Context) error {
				progress(start, clock().Add(3*time.Second))
				<-ctx.Done()
				return ctx.Err()
			}}
		},
	})
	defer e.Close()
	e.SetControls([]sink.Sink{member})
	e.SetTargets(context.Background(), []sink.Target{{PlayerID: "study", Sink: fake.New("study")}})
	if err := e.Load(context.Background(), []int64{1}, 0, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	waitState(t, e, StatePlaying)
	tick.Store(initial.Add(8 * time.Second).UnixNano())
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		member.mu.Lock()
		calls, target := member.calls, member.target
		member.mu.Unlock()
		if calls > 0 {
			if target < 15*time.Second || target > 17*time.Second {
				t.Fatal("wrong media clock", target)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("mixed engine never aligned its pulling member")
}

type seekTrackingControl struct {
	*ctrlSink
	seeks atomic.Int32
}

func (s *seekTrackingControl) Seek(uint64) error { s.seeks.Add(1); return nil }
func TestSeekReloadDoesNotSeekTheOldControlSession(t *testing.T) {
	e := newDelegatedEngine(t, staticResolver{1: {SongID: 1, Duration: time.Minute}})
	defer e.Close()
	member := &seekTrackingControl{ctrlSink: newCtrlSink("dlna")}
	e.SetControls([]sink.Sink{member})
	if err := e.Load(context.Background(), []int64{1}, 0, 0); err != nil {
		t.Fatal(err)
	}
	waitState(t, e, StatePlaying)
	if err := e.Seek(context.Background(), 20*time.Second); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		opens, _, _, _ := member.snapshot()
		if opens >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("new seek session was not loaded")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if member.seeks.Load() != 0 {
		t.Fatal("sent a second seek to an old/reloading session")
	}
	if member.lastPlan().Position != 20*time.Second {
		t.Fatal("reload lost seek position")
	}
}
