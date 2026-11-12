package player

import (
	"context"
	"reflect"
	"testing"
)

func TestEngineProgressRestoresOnlyMatchingQueue(t *testing.T) {
	for _, match := range []bool{true, false} {
		items := []int64{1, 2, 3}
		if !match {
			items = []int64{3, 2, 1}
		}
		store := &restoreStore{queue: []int64{1, 2, 3}, state: StateSnapshot{SongID: 3, State: "paused", QueueProgress: &QueueSnapshot{Items: items, Started: []int{0, 2}}}}
		e := newRestoreEngine(t, store)
		if err := e.Restore(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := len(e.Status().Queue.Started); (match && got != 2) || (!match && got != 0) {
			t.Fatalf("match=%v: %d", match, got)
		}
	}
}

func TestEngineReportsCountActualPlaybackNotSelection(t *testing.T) {
	e := newRestoreEngine(t, &restoreStore{})
	_ = e.SetQueue([]int64{1, 2, 3}, 2)
	e.MirrorReport(3, "paused", 0)
	if len(e.Status().Queue.Started) != 0 {
		t.Fatal("paused selection counted")
	}
	e.MirrorReport(3, "playing", 0)
	_ = e.SetQueue([]int64{1, 2, 3}, 0)
	e.MirrorReport(1, "playing", 0)
	if got := e.Status().Queue.Started; !reflect.DeepEqual(got, []int{0, 2}) {
		t.Fatal(got)
	}
	_ = e.SetQueueWithProgress([]int64{1, 2, 3}, 2, []int{})
	e.MirrorReport(3, "playing", 0)
	if got := e.Status().Queue.Started; !reflect.DeepEqual(got, []int{2}) {
		t.Fatal("fresh round", got)
	}
}

func TestQueueProgressTracksActualOccurrences(t *testing.T) {
	q := NewQueue()
	q.Replace([]int64{1, 1, 2, 3}, 3)
	if len(q.Snapshot().Started) != 0 {
		t.Fatal("selection is not playback")
	}
	q.MarkStarted(3)
	_ = q.Jump(1)
	q.MarkStarted(1)
	q.MarkStarted(999) // late report for an unrelated song
	q.SetShuffle(true)
	q.SetShuffle(false)
	if got := q.Snapshot().Started; !reflect.DeepEqual(got, []int{1, 3}) {
		t.Fatal(got)
	}
	_ = q.Move(1, 0)
	if q.Current() != 0 {
		t.Fatal("duplicate occurrence moved current incorrectly")
	}
	q.InsertNextMany(4, 5)
	_ = q.Remove(3)
	if got := q.Snapshot().Started; !reflect.DeepEqual(got, []int{0, 4}) {
		t.Fatal(got)
	}
	restored := NewQueue()
	restored.Restore(q.Snapshot())
	if !reflect.DeepEqual(q.Snapshot(), restored.Snapshot()) {
		t.Fatal("lost progress")
	}
	q.Replace([]int64{1, 2}, 1)
	if len(q.Snapshot().Started) != 0 {
		t.Fatal("replacement must start fresh")
	}
}

func TestQueueMirrorPreservesProgressAcrossIndexReports(t *testing.T) {
	q := NewQueue()
	q.Replace([]int64{1, 2, 3}, 2)
	q.MarkStarted(3)
	q.ReplaceKeepingProgress([]int64{1, 2, 3, 4}, 0)
	q.MarkStarted(1)
	if got := q.Snapshot().Started; !reflect.DeepEqual(got, []int{0, 2}) {
		t.Fatal(got)
	}
}
