package player

import (
	"context"
	"errors"
	"songloft/internal/player/sink"
	"testing"
	"time"
)

type joinPush struct {
	*ctrlSink
	plan sink.StreamPlan
	ctx  context.Context
	at   time.Time
	fail bool
}

func (s *joinPush) Open(ctx context.Context, p sink.StreamPlan) error {
	s.plan = p
	s.ctx = ctx
	if s.fail {
		return errors.New("handshake failed")
	}
	return nil
}
func (s *joinPush) Start(at time.Time) error    { s.at = at; return nil }
func (s *joinPush) WriteFrame(sink.Frame) error { return nil }

func hotJoinEngine() *Engine {
	e := NewEngine("group", "join", EngineDeps{})
	e.running = true
	e.runCtx = context.Background()
	e.curPlan = sink.StreamPlan{SongID: 2, Position: 3 * time.Second}
	e.format.SampleRate = 44100
	e.planBase = 44100 * 100
	e.cursor = e.planBase + 44100*12
	e.timeline = NewTimeline(time.Now(), 44100, 2*time.Second)
	return e
}
func TestHotJoinControlLoadsCurrentTrackOnce(t *testing.T) {
	e := hotJoinEngine()
	web := newCtrlSink("web")
	if err := e.SetControls([]sink.Sink{web}); err != nil {
		t.Fatal(err)
	}
	if p := web.lastPlan(); p.SongID != 2 || p.Position != 15*time.Second {
		t.Fatalf("hot join plan: %+v", p)
	}
	if err := e.SetControls([]sink.Sink{web}); err != nil {
		t.Fatal(err)
	}
	opens, starts, _, _ := web.snapshot()
	if opens != 1 || starts != 1 {
		t.Fatalf("load/start=%d/%d", opens, starts)
	}
	if err := e.SetControls(nil); err != nil {
		t.Fatal(err)
	}
	_, _, stops, _ := web.snapshot()
	if stops != 1 {
		t.Fatalf("removed control stopped %d times", stops)
	}
}
func TestHotJoinPushUsesAbsoluteTimelineAndPlaybackContext(t *testing.T) {
	e := hotJoinEngine()
	s := &joinPush{ctrlSink: newCtrlSink("cocoon")}
	request, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.AttachTarget(request, sink.Target{PlayerID: s.ID(), Sink: s}); err != nil {
		t.Fatal(err)
	}
	if s.ctx.Err() != nil {
		t.Fatal("join used expired HTTP context")
	}
	if s.plan.Position != 15*time.Second || s.plan.SampleIndex == nil || *s.plan.SampleIndex != e.cursor {
		t.Fatalf("plan=%+v", s.plan)
	}
	if !s.at.Equal(e.timeline.At(uint64(e.cursor))) {
		t.Fatal("joined a different timeline")
	}
}
func TestHotJoinFailureDoesNotAttachAndStopsSession(t *testing.T) {
	e := hotJoinEngine()
	s := &joinPush{ctrlSink: newCtrlSink("bad"), fail: true}
	if err := e.AttachTarget(context.Background(), sink.Target{PlayerID: s.ID(), Sink: s}); err == nil {
		t.Fatal("missing join failure")
	}
	if e.targets.Len() != 0 {
		t.Fatal("failed target attached")
	}
	_, _, stops, _ := s.snapshot()
	if stops != 1 {
		t.Fatal("failed session not stopped")
	}
}

func TestManagerFailedJoinReturnsErrorAndRestoresSoloZone(t *testing.T) {
	ctx := context.Background()
	m := NewManager(ManagerDeps{})
	caps := Caps{Codecs: []Codec{CodecL16}, MaxSampleRate: 44100, SyncTier: SyncExact}
	m.players["study"] = &Player{ID: "study", Caps: caps}
	m.players["cocoon"] = &Player{ID: "cocoon", Caps: caps}
	m.groups["main"] = &Group{ID: "main", MasterID: "study", Members: []Member{{PlayerID: "study"}}}
	m.groups["old"] = &Group{ID: "old", MasterID: "cocoon", Members: []Member{{PlayerID: "cocoon"}}}
	e := hotJoinEngine()
	m.engines["group:main"] = e
	primary := &joinPush{ctrlSink: newCtrlSink("study")}
	e.targets.Add(sink.Target{PlayerID: "study", Sink: primary})
	m.sinks["study"] = primary
	m.sinks["cocoon"] = &joinPush{ctrlSink: newCtrlSink("cocoon"), fail: true}
	if _, err := m.AddMember(ctx, "main", "cocoon"); err == nil {
		t.Fatal("failed handshake reported a successful group")
	}
	g, err := m.GetGroup("main")
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Members) != 1 || g.Members[0].PlayerID != "study" {
		t.Fatalf("failed member retained: %+v", g.Members)
	}
	if id := m.GroupIDOf("cocoon"); id == "" || id == "main" {
		t.Fatalf("solo zone not restored: %s", id)
	}
	if _, ok := e.targets.Get("study"); !ok {
		t.Fatal("primary speaker removed")
	}
	_, _, stops, _ := primary.snapshot()
	if stops != 0 {
		t.Fatal("join failure stopped the primary")
	}
}
