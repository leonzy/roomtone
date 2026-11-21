package player

import (
	"testing"
	"time"
)

func TestHistoryStartIndependentOfPersistedQueueProgress(t *testing.T) {
	e := NewEngine("group", "living", EngineDeps{})
	e.queue.Replace([]int64{42, 42}, 0)
	e.queue.MarkStarted(42)

	if !e.markHistoryStarted(42) {
		t.Fatal("restored queue progress must not suppress a new listening event")
	}
	if e.markHistoryStarted(42) {
		t.Fatal("same queue occurrence must not emit twice during seek or resume")
	}
	if !e.queue.Next(false) {
		t.Fatal("expected next queue occurrence")
	}
	if !e.markHistoryStarted(42) {
		t.Fatal("same song at another queue occurrence must emit")
	}
	e.stopLocked()
	if e.markHistoryStarted(42) {
		t.Fatal("internal transport rebuild must not create a new listening session")
	}
	e.Stop()
	if !e.markHistoryStarted(42) {
		t.Fatal("a new playback session after stop must emit")
	}
}

func TestMirrorReportHistoryDoesNotDeadlock(t *testing.T) {
	e := NewEngine("group", "living", EngineDeps{})
	e.queue.Replace([]int64{42}, 0)
	done := make(chan struct{})
	go func() {
		e.MirrorReport(42, "playing", 0)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("MirrorReport must not deadlock while recording history")
	}
}
