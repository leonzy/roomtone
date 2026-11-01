package player

import (
	"context"
	"songloft/internal/player/sink"
	"testing"
	"time"
)

func TestControlCalibrationSchedulesEachMemberAndKeepsActiveOffset(t *testing.T) {
	e := NewEngine("group", "calibration", EngineDeps{})
	a, b := &joinPush{ctrlSink: newCtrlSink("dlna")}, &joinPush{ctrlSink: newCtrlSink("web")}
	at := time.Now().Add(2 * time.Second)
	e.SetControlDelays(map[string]time.Duration{"dlna": time.Second, "web": -500 * time.Millisecond})
	if err := e.startControl(a, at); err != nil {
		t.Fatal(err)
	}
	if err := e.startControl(b, at); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	aa := a.at
	a.mu.Unlock()
	b.mu.Lock()
	ba := b.at
	b.mu.Unlock()
	if aa.Sub(at) != time.Second || ba.Sub(at) != -500*time.Millisecond {
		t.Fatalf("control calibration not scheduled: %v / %v", aa.Sub(at), ba.Sub(at))
	}
	e.SetControlDelays(map[string]time.Duration{"dlna": 0})
	if e.activeControlDelay("dlna") != time.Second {
		t.Fatal("unscheduled setting changed active feedback")
	}
}

func TestControlSyncPreservesMemberCalibration(t *testing.T) {
	member := &aligningControl{ctrlSink: newCtrlSink("dlna")}
	member.setPos(8 * time.Second)
	loop := ControlSyncLoop{Members: func() []sink.Sink { return []sink.Sink{member} }, Reference: func() (int64, time.Duration, bool) { return 2, 10 * time.Second, true }, Delay: func(string) time.Duration { return 2 * time.Second }}
	loop.CheckOnce(context.Background())
	loop.CheckOnce(context.Background())
	if member.calls != 0 {
		t.Fatal("feedback undid saved calibration")
	}
	member.setPos(4 * time.Second)
	loop.CheckOnce(context.Background())
	loop.CheckOnce(context.Background())
	if member.calls != 1 || member.target != 8*time.Second {
		t.Fatalf("alignment ignored delay: %v / %v", member.calls, member.target)
	}
}
