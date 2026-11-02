package player

import (
	"context"
	"encoding/binary"
	"log/slog"
	"songloft/internal/player/dsp"
	"songloft/internal/player/sink"
	"songloft/internal/player/sink/fake"
	"testing"
)

func channelPCM(l, r int16) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint16(b, uint16(l))
	binary.LittleEndian.PutUint16(b[2:], uint16(r))
	return b
}
func checkChannels(t *testing.T, b []byte, l, r int16) {
	t.Helper()
	if int16(binary.LittleEndian.Uint16(b)) != l || int16(binary.LittleEndian.Uint16(b[2:])) != r {
		t.Fatalf("unexpected samples: %v", b)
	}
}
func TestGroupChannelProcessorAndSoloBypass(t *testing.T) {
	for _, tc := range []struct {
		mode string
		l, r int16
	}{{"left", 12000, 12000}, {"right", -4000, -4000}, {"mono", 4000, 4000}, {"stereo", 12000, -4000}} {
		members := []Member{{PlayerID: "a", ChannelMode: tc.mode}, {PlayerID: "b"}}
		p := groupProcessor(nil, members, "a")
		b := channelPCM(12000, -4000)
		if p != nil {
			p.ProcessInPlace(b)
		}
		checkChannels(t, b, tc.l, tc.r)
		p = groupProcessor(nil, members[:1], "a")
		b = channelPCM(12000, -4000)
		if p != nil {
			p.ProcessInPlace(b)
		}
		checkChannels(t, b, 12000, -4000)
	}
	b := channelPCM(32767, 32767)
	channelProcessor{mode: "mono"}.ProcessInPlace(b)
	checkChannels(t, b, 32767, 32767)
	b = channelPCM(-32768, -32768)
	channelProcessor{mode: "mono"}.ProcessInPlace(b)
	checkChannels(t, b, -32768, -32768)
}
func TestGroupChannelsReachIndependentTargets(t *testing.T) {
	a, b := fake.New("a"), fake.New("b")
	g := &Group{ID: "g", MasterID: "a", Members: []Member{{PlayerID: "a", ChannelMode: "stereo"}, {PlayerID: "b", ChannelMode: "stereo"}}}
	e := NewEngine("group", "g", EngineDeps{Logger: slog.Default()})
	m := &Manager{deps: ManagerDeps{Logger: slog.Default()}, players: map[string]*Player{"a": {ID: "a", Name: "A", Kind: KindRAOP, Caps: a.Caps()}, "b": {ID: "b", Name: "B", Kind: KindRAOP, Caps: b.Caps()}}, groups: map[string]*Group{"g": g}, engines: map[string]*Engine{"group:g": e}, sinks: map[string]sink.Sink{"a": a, "b": b}, eqChains: map[string]*dsp.EQ{}}
	for _, pair := range []struct{ id, mode string }{{"a", "left"}, {"b", "right"}} {
		if _, err := m.UpdateMember(context.Background(), "g", pair.id, 0, 0, pair.mode); err != nil {
			t.Fatal(err)
		}
	}
	for _, pair := range []struct {
		id   string
		want int16
	}{{"a", 12000}, {"b", -4000}} {
		target, ok := e.Targets().Get(pair.id)
		if !ok {
			t.Fatal("missing target")
		}
		pcm := channelPCM(12000, -4000)
		target.EQ.ProcessInPlace(pcm)
		checkChannels(t, pcm, pair.want, pair.want)
	}
	// Legacy calibration updates must retain the chosen channel.
	if _, err := m.UpdateMember(context.Background(), "g", "a", 100, 3); err != nil {
		t.Fatal(err)
	}
	if g.Members[0].ChannelMode != "left" {
		t.Fatal("legacy edit lost role")
	}
	// Remaining solo target must immediately receive unmodified stereo again.
	m.mu.Lock()
	g.Members = g.Members[:1]
	m.mu.Unlock()
	if err := m.rebuildSinks(context.Background(), "g"); err != nil {
		t.Fatal(err)
	}
	target, _ := e.Targets().Get("a")
	pcm := channelPCM(12000, -4000)
	target.EQ.ProcessInPlace(pcm)
	checkChannels(t, pcm, 12000, -4000)
}

func TestChannelAssignmentUngroupAndRegroupDefaults(t *testing.T) {
	m := NewManager(ManagerDeps{})
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if err := m.RegisterPlayer(ctx, Player{ID: id, Kind: KindRAOP, Name: id}); err != nil {
			t.Fatal(err)
		}
	}
	g, err := m.CreateGroup(ctx, "pair", []string{"a", "b"}, "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.UpdateMember(ctx, g.ID, "a", 0, 0, "left"); err != nil {
		t.Fatal(err)
	}
	if err = m.RemoveMember(ctx, g.ID, "b", "solo"); err != nil {
		t.Fatal(err)
	}
	solo, err := m.GetGroup(g.ID)
	if err != nil || solo.Members[0].ChannelMode != "stereo" {
		t.Fatalf("solo: %+v %v", solo, err)
	}
	pair, err := m.AddMember(ctx, g.ID, "b")
	if err != nil {
		t.Fatal(err)
	}
	for _, member := range pair.Members {
		if member.ChannelMode != "stereo" {
			t.Fatal("regrouping automatically reactivated a split channel")
		}
	}
}
