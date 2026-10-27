package player

import (
	"context"
	"testing"
	"time"

	"songloft/internal/player/sink"
	"songloft/internal/player/sink/fake"
)

func TestPauseClosesPushSession(t *testing.T) {
	e := NewEngine("group", "pause", EngineDeps{})
	s := fake.New("speaker")
	if err := e.SetTargets(context.Background(), []sink.Target{{PlayerID: s.ID(), Sink: s}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Open(context.Background(), sink.StreamPlan{}); err != nil {
		t.Fatal(err)
	}
	// Buffering is a valid pause source; the audio session is already open.
	e.transport.Load(1, 30*time.Second)
	if err := e.transport.MarkBuffering(); err != nil {
		t.Fatal(err)
	}
	if err := e.Pause(); err != nil {
		t.Fatal(err)
	}
	if !s.IsStopped() {
		t.Fatal("pause left push transport session open")
	}
}

func TestResumeTimelineDoesNotWaitElapsedTrackTime(t *testing.T) {
	now := time.Now()
	e := NewEngine("group", "resume", EngineDeps{Clock: func() time.Time { return now }})
	e.mu.Lock()
	e.startLocked(context.Background(), 90*time.Second)
	got := e.timeline.At(uint64(90 * e.format.SampleRate))
	e.stopLocked()
	e.mu.Unlock()
	if !got.Equal(now.Add(e.deps.Latency)) {
		t.Fatalf("first resumed sample scheduled at %v instead of now + latency", got)
	}
}
