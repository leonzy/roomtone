package player

import (
	"context"
	"io"
	"songloft/internal/player/sink"
	"sync"
	"testing"
	"time"
)

type blockingOpenControl struct {
	*ctrlSink
	entered, release chan struct{}
}

func (c *blockingOpenControl) Open(ctx context.Context, plan sink.StreamPlan) error {
	close(c.entered)
	<-c.release
	return c.ctrlSink.Open(ctx, plan)
}

type signaledStartPush struct {
	*joinPush
	started chan struct{}
}

func (p *signaledStartPush) Start(at time.Time) error {
	close(p.started)
	return p.joinPush.Start(at)
}

func TestMixedStartWaitsForPullPreparation(t *testing.T) {
	ctx := context.Background()
	e := NewEngine("group", "prepare", EngineDeps{
		Resolver: staticResolver{1: {SongID: 1}}, Decoder: completionDecoderFactory{},
		Runners: func(*Timeline, *sink.Set, int64, func(int64, time.Time)) FrameRunner {
			return completionRunner{func(context.Context) error { return io.EOF }}
		},
	})
	push := &signaledStartPush{joinPush: &joinPush{ctrlSink: newCtrlSink("study")}, started: make(chan struct{})}
	pull := &blockingOpenControl{ctrlSink: newCtrlSink("xiaomi"), entered: make(chan struct{}), release: make(chan struct{})}
	e.targets.Store([]sink.Target{{PlayerID: "study", Sink: push}})
	e.controls = []sink.Sink{pull}
	e.queue.Replace([]int64{1}, 0)
	e.runCtx = ctx
	done := make(chan struct{})
	go func() { e.run(ctx, 0); close(done) }()
	select {
	case <-pull.entered:
	case <-time.After(time.Second):
		t.Fatal("pull preparation never started")
	}
	startedEarly := false
	select {
	case <-push.started:
		startedEarly = true
	default:
	}
	close(pull.release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("playback did not finish")
	}
	if startedEarly {
		t.Fatal("push started before pull preparation completed")
	}
}

type blockingStopPush struct {
	*joinPush
	entered, release chan struct{}
	once             sync.Once
}

func (p *blockingStopPush) Stop() error {
	p.once.Do(func() { close(p.entered) })
	<-p.release
	return nil
}

type signaledPauseControl struct {
	*ctrlSink
	paused chan struct{}
	once   sync.Once
}

func (c *signaledPauseControl) Pause() error { c.once.Do(func() { close(c.paused) }); return nil }

func TestPauseNotifiesPullBeforeSlowPushTeardownFinishes(t *testing.T) {
	e := hotJoinEngine()
	e.transport.Load(1, 0)
	e.transport.MarkBuffering()
	e.transport.Play()
	push := &blockingStopPush{joinPush: &joinPush{ctrlSink: newCtrlSink("study")}, entered: make(chan struct{}), release: make(chan struct{})}
	pull := &signaledPauseControl{ctrlSink: newCtrlSink("xiaomi"), paused: make(chan struct{})}
	e.targets.Store([]sink.Target{{PlayerID: "study", Sink: push}})
	e.controls = []sink.Sink{pull}
	done := make(chan struct{})
	go func() { e.Pause(); close(done) }()
	select {
	case <-push.entered:
	case <-time.After(time.Second):
		t.Fatal("push did not begin stopping")
	}
	parallel := false
	select {
	case <-pull.paused:
		parallel = true
	case <-time.After(100 * time.Millisecond):
	}
	close(push.release)
	<-done
	if !parallel {
		t.Fatal("pull pause waited for the push teardown reply")
	}
}
