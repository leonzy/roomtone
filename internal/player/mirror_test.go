package player

import (
	"context"
	"testing"
	"time"

	"songloft/internal/player/sink"
)

// TestMirrorReportAdoptsClientPlayback 客户端自己起播时，组状态必须跟着变。
//
// 回归：用户直接点客户端播放条时服务端既没下发指令也没有解码协程，组状态
// 一直停在 idle，UI 上就是「明明在响却显示空闲」。
func TestMirrorReportAdoptsClientPlayback(t *testing.T) {
	e := newRestoreEngine(t, &restoreStore{})
	e.SetControls([]sink.Sink{newCtrlSink("web")})

	e.MirrorReport(7, "playing", 12*time.Second)
	st := e.Status()
	if st.State != StatePlaying {
		t.Fatalf("state = %s, want playing", st.State)
	}
	if st.SongID != 7 {
		t.Fatalf("song_id = %d, want 7", st.SongID)
	}
	if st.Position < 11.9 || st.Position > 12.5 {
		t.Fatalf("position = %.2f, want ≈12", st.Position)
	}

	e.MirrorReport(7, "paused", 20*time.Second)
	st = e.Status()
	if st.State != StatePaused {
		t.Fatalf("state = %s, want paused", st.State)
	}
	if st.Position < 19.9 || st.Position > 20.1 {
		t.Fatalf("position = %.2f, want ≈20", st.Position)
	}

	e.MirrorReport(0, "idle", 0)
	if st = e.Status(); st.State != StateIdle {
		t.Fatalf("state = %s, want idle", st.State)
	}
}

// TestMirrorReportIgnoredWhileServerDrives 服务端在驱动时上报只用于位置校正，
// 不能反过来改状态机。
func TestMirrorReportIgnoredWhileServerDrives(t *testing.T) {
	cs := newCtrlSink("web")
	e := newDelegatedEngine(t, staticResolver{
		1: {SongID: 1, Title: "a", Duration: 30 * time.Second},
	})
	e.SetControls([]sink.Sink{cs})

	if err := e.Load(context.Background(), []int64{1}, 0, 0); err != nil {
		t.Fatalf("Load: %v", err)
	}
	waitState(t, e, StatePlaying)

	e.MirrorReport(99, "paused", 5*time.Second)
	st := e.Status()
	if st.State != StatePlaying {
		t.Fatalf("state = %s, want playing（服务端驱动时忽略上报）", st.State)
	}
	if st.SongID != 1 {
		t.Fatalf("song_id = %d, want 1", st.SongID)
	}
}
