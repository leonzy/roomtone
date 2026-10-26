package player

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"songloft/internal/player/sink"
)

// restoreStore 只实现 Restore 用到的四个方法。
type restoreStore struct {
	queue []int64
	state StateSnapshot
}

func (s *restoreStore) SaveQueue(context.Context, string, string, []int64) error { return nil }
func (s *restoreStore) LoadQueue(context.Context, string, string) ([]int64, error) {
	return append([]int64(nil), s.queue...), nil
}
func (s *restoreStore) SaveState(context.Context, string, string, StateSnapshot) error { return nil }
func (s *restoreStore) LoadState(context.Context, string, string) (StateSnapshot, error) {
	return s.state, nil
}

func newRestoreEngine(t *testing.T, st *restoreStore) *Engine {
	t.Helper()
	return NewEngine("group", "g1", EngineDeps{
		Logger:   slog.Default(),
		Resolver: staticResolver{1: {SongID: 1, Title: "a", Duration: 30 * time.Second}, 2: {SongID: 2, Title: "b", Duration: 30 * time.Second}},
		Store:    st,
	})
}

// TestRestoreAfterStopDoesNotStickInLoading 停止后落库的是 song_id=0 / idle，
// 恢复时不能把它当成"待续播"。
//
// 回归：以前 Restore 无条件 Load(st.SongID) + Pause()。song_id=0 时状态机被推进
// loading，而 Pause 不接受 loading（返回 ErrInvalidTransition），状态永久停在
// loading；Play 在 loading 下是空操作，于是区域再也起不来——线上表现为
// 「点了播放没反应，区域一直显示空闲」。
func TestRestoreAfterStopDoesNotStickInLoading(t *testing.T) {
	st := &restoreStore{
		queue: []int64{1, 2},
		state: StateSnapshot{SongID: 0, State: string(StateIdle)},
	}
	e := newRestoreEngine(t, st)
	cs := newCtrlSink("web")
	e.SetControls([]sink.Sink{cs})

	if err := e.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := e.Status().State; got != StateIdle {
		t.Fatalf("恢复后 state = %s, want idle", got)
	}

	// 关键：恢复后必须还能起播。
	if err := e.Play(context.Background()); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, e, StatePlaying)
	if got := e.Status().SongID; got != 1 {
		t.Fatalf("song_id = %d, want 1", got)
	}
}

// TestRestorePausedResumesFromSavedPosition 存过暂停态时按原曲目与位置恢复。
func TestRestorePausedResumesFromSavedPosition(t *testing.T) {
	st := &restoreStore{
		queue: []int64{1, 2},
		state: StateSnapshot{SongID: 2, Position: 5, State: string(StatePaused)},
	}
	e := newRestoreEngine(t, st)
	cs := newCtrlSink("web")
	e.SetControls([]sink.Sink{cs})

	if err := e.Restore(context.Background()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	got := e.Status()
	if got.State != StatePaused {
		t.Fatalf("state = %s, want paused", got.State)
	}
	if got.SongID != 2 {
		t.Fatalf("song_id = %d, want 2", got.SongID)
	}
	if got.Position < 4.9 || got.Position > 5.1 {
		t.Fatalf("position = %.2f, want ≈5", got.Position)
	}

	if err := e.Play(context.Background()); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, e, StatePlaying)
	if plan := cs.lastPlan(); plan.SongID != 2 {
		t.Fatalf("恢复播放的 load song_id = %d, want 2", plan.SongID)
	}
}

// TestPlayRecoversFromDeadLoading loading 但没有解码协程是死状态，Play 必须能救回来。
func TestPlayRecoversFromDeadLoading(t *testing.T) {
	e := newRestoreEngine(t, &restoreStore{})
	cs := newCtrlSink("web")
	e.SetControls([]sink.Sink{cs})

	// 人为制造死状态：Load 会起协程，这里直接改状态机模拟"协程已退出"。
	e.queue.Replace([]int64{1}, 0)
	e.transport.Load(0, 0)
	if got := e.Status().State; got != StateLoading {
		t.Fatalf("前置状态 = %s, want loading", got)
	}

	if err := e.Play(context.Background()); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitState(t, e, StatePlaying)
	if got := e.Status().SongID; got != 1 {
		t.Fatalf("song_id = %d, want 1", got)
	}
}
