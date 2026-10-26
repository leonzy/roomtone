package player

import (
	"context"
	"testing"
	"time"

	"songloft/internal/player/sink"
)

// TestSetQueueReplacesWithoutPlaying 客户端推队列只改队列，不启动解码。
//
// 区域拥有队列：客户端是编辑器，服务端只保存「这个区域排了什么」。
// 推队列不能顺带出声，否则切区域会把上一个区域的歌播出来。
func TestSetQueueReplacesWithoutPlaying(t *testing.T) {
	e := newRestoreEngine(t, &restoreStore{})
	cs := newCtrlSink("web")
	e.SetControls([]sink.Sink{cs})

	if err := e.SetQueue([]int64{2, 1}, 1); err != nil {
		t.Fatalf("SetQueue: %v", err)
	}

	st := e.Status()
	if st.State != StateIdle {
		t.Fatalf("state = %s, want idle（推队列不能起播）", st.State)
	}
	if got := st.Queue.Items; len(got) != 2 || got[0] != 2 || got[1] != 1 {
		t.Fatalf("queue = %v, want [2 1]", got)
	}
	if st.Queue.Current != 1 {
		t.Fatalf("current = %d, want 1", st.Queue.Current)
	}
	if st.SongID != 1 {
		t.Fatalf("song_id = %d, want 1", st.SongID)
	}
}

// TestSetQueueClampsIndex 下标越界时落到 0，而不是报错或留空。
func TestSetQueueClampsIndex(t *testing.T) {
	e := newRestoreEngine(t, &restoreStore{})

	if err := e.SetQueue([]int64{1, 2}, 9); err != nil {
		t.Fatalf("SetQueue: %v", err)
	}
	if got := e.Status().Queue.Current; got != 0 {
		t.Fatalf("current = %d, want 0", got)
	}
}

// TestSetQueueEmptyClears 空数组表示清空队列（用户删光了队列）。
func TestSetQueueEmptyClears(t *testing.T) {
	e := newRestoreEngine(t, &restoreStore{})

	if err := e.SetQueue([]int64{1, 2}, 0); err != nil {
		t.Fatalf("SetQueue: %v", err)
	}
	if err := e.SetQueue(nil, 0); err != nil {
		t.Fatalf("SetQueue(nil): %v", err)
	}

	st := e.Status()
	if len(st.Queue.Items) != 0 {
		t.Fatalf("queue = %v, want empty", st.Queue.Items)
	}
	if st.Queue.Current != -1 {
		t.Fatalf("current = %d, want -1", st.Queue.Current)
	}
	if st.State != StateIdle {
		t.Fatalf("state = %s, want idle", st.State)
	}
}

// TestSetQueueStopsDecodeWhenCurrentChanges 正在解码时换掉当前曲目，
// 必须停掉解码并落到 paused，否则服务端会继续播一首已经不在队列里的歌。
func TestSetQueueStopsDecodeWhenCurrentChanges(t *testing.T) {
	e := newDelegatedEngine(t, staticResolver{
		1: {SongID: 1, Title: "a", Duration: 30 * time.Second},
		2: {SongID: 2, Title: "b", Duration: 30 * time.Second},
	})
	e.SetControls([]sink.Sink{newCtrlSink("web")})

	if err := e.Load(context.Background(), []int64{1}, 0, 0); err != nil {
		t.Fatalf("Load: %v", err)
	}
	waitState(t, e, StatePlaying)

	if err := e.SetQueue([]int64{2}, 0); err != nil {
		t.Fatalf("SetQueue: %v", err)
	}

	st := e.Status()
	if st.State != StatePaused {
		t.Fatalf("state = %s, want paused（换歌要停解码）", st.State)
	}
	if st.SongID != 2 {
		t.Fatalf("song_id = %d, want 2", st.SongID)
	}
	if st.Position > 0.5 {
		t.Fatalf("position = %.2f, want 0", st.Position)
	}
}

// TestSetQueueKeepsPlayingSameSong 队列里其它位置变了但当前曲目没变时，
// 不能打断正在播放的歌。
func TestSetQueueKeepsPlayingSameSong(t *testing.T) {
	e := newDelegatedEngine(t, staticResolver{
		1: {SongID: 1, Title: "a", Duration: 30 * time.Second},
		2: {SongID: 2, Title: "b", Duration: 30 * time.Second},
	})
	e.SetControls([]sink.Sink{newCtrlSink("web")})

	if err := e.Load(context.Background(), []int64{1}, 0, 0); err != nil {
		t.Fatalf("Load: %v", err)
	}
	waitState(t, e, StatePlaying)

	if err := e.SetQueue([]int64{1, 2}, 0); err != nil {
		t.Fatalf("SetQueue: %v", err)
	}

	st := e.Status()
	if st.State != StatePlaying {
		t.Fatalf("state = %s, want playing（当前曲目没变不该停）", st.State)
	}
	if len(st.Queue.Items) != 2 {
		t.Fatalf("queue = %v, want 2 items", st.Queue.Items)
	}
}

// TestSetQueueKeepsPositionForSameSong 队列编辑不能清零正在播的进度。
//
// 客户端每次改队列都会 PUT /groups/{id}/queue，SetQueue 在未解码时走
// SelectSong。如果同一首歌也把 base 归零，切区域回来就“丢进度”。
func TestSetQueueKeepsPositionForSameSong(t *testing.T) {
	e := newRestoreEngine(t, &restoreStore{})
	cs := newCtrlSink("web")
	e.SetControls([]sink.Sink{cs})

	if err := e.SetQueue([]int64{1, 2}, 0); err != nil {
		t.Fatalf("SetQueue: %v", err)
	}
	// 模拟客户端上报：正在播第 1 首的 42s。
	e.MirrorReport(1, "playing", 42*time.Second)
	if got := e.Status().Position; got < 41 || got > 43 {
		t.Fatalf("position = %.1f, want ~42", got)
	}

	// 再推一次队列（当前曲目没变，只是追加了一首）。
	if err := e.SetQueue([]int64{1, 2, 3}, 0); err != nil {
		t.Fatalf("SetQueue: %v", err)
	}
	st := e.Status()
	if st.SongID != 1 {
		t.Fatalf("song_id = %d, want 1", st.SongID)
	}
	if st.Position < 41 || st.Position > 44 {
		t.Fatalf("position = %.1f, want ~42（队列编辑不能清零进度）", st.Position)
	}
	if st.State != StatePlaying {
		t.Fatalf("state = %s, want playing", st.State)
	}
}

// TestSelectSongResetsPositionOnRealChange 真正换歌时位置必须归零。
func TestSelectSongResetsPositionOnRealChange(t *testing.T) {
	e := newRestoreEngine(t, &restoreStore{})
	cs := newCtrlSink("web")
	e.SetControls([]sink.Sink{cs})

	if err := e.SetQueue([]int64{1, 2}, 0); err != nil {
		t.Fatalf("SetQueue: %v", err)
	}
	e.MirrorReport(1, "playing", 42*time.Second)

	if err := e.SetQueue([]int64{1, 2}, 1); err != nil {
		t.Fatalf("SetQueue: %v", err)
	}
	st := e.Status()
	if st.SongID != 2 {
		t.Fatalf("song_id = %d, want 2", st.SongID)
	}
	if st.Position != 0 {
		t.Fatalf("position = %.1f, want 0（换歌必须从头）", st.Position)
	}
}
