package discovery

import (
	"context"
	"errors"
	"net"
	"songloft/internal/player"
	"testing"
)

type onlineRegistry struct{ *fakeRegistry }

func (r onlineRegistry) ListPlayers() []player.Player {
	var out []player.Player
	for _, p := range r.players {
		out = append(out, p)
	}
	return out
}
func (r onlineRegistry) SetOnline(_ context.Context, id string, online bool) {
	p := r.players[id]
	p.Online = online
	r.players[id] = p
}
func TestMissingSpeakersGoOfflineOnlyAfterSuccessfulScans(t *testing.T) {
	reg := onlineRegistry{newFakeRegistry()}
	reg.players["bridge"] = player.Player{ID: "bridge", Kind: player.KindRAOP, Online: true}
	reg.players["web"] = player.Player{ID: "web", Kind: player.KindLocal, Online: true}
	scanner := &fakeScanner{kind: player.KindRAOP}
	d := New(Options{Registry: reg, Scanners: []Scanner{scanner}, Protocols: []string{"raop"}})
	ctx := context.Background()
	d.Scan(ctx)
	if !reg.players["bridge"].Online {
		t.Fatal("single missing advertisement retired speaker")
	}
	scanner.err = errors.New("network unavailable")
	d.Scan(ctx)
	if !reg.players["bridge"].Online {
		t.Fatal("failed scan retired speaker")
	}
	scanner.err = nil
	d.Scan(ctx)
	if reg.players["bridge"].Online || !reg.players["web"].Online {
		t.Fatal("offline handling did not respect successful protocol scan")
	}
	scanner.list = []Speaker{{ID: "bridge", Kind: player.KindRAOP}}
	d.Scan(ctx)
	if d.misses["bridge"] != 0 {
		t.Fatal("returned speaker retains missing counter")
	}
}

func TestMissingMulticastDoesNotHideReachableSpeaker(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	reg := onlineRegistry{newFakeRegistry()}
	reg.players["speaker"] = player.Player{ID: "speaker", Kind: player.KindRAOP, Address: listener.Addr().String(), Online: true}
	d := New(Options{Registry: reg, Scanners: []Scanner{&fakeScanner{kind: player.KindRAOP}}, Protocols: []string{"raop"}})
	d.Scan(context.Background())
	d.Scan(context.Background())
	if !reg.players["speaker"].Online {
		t.Fatal("lost multicast response hid reachable speaker")
	}
	listener.Close()
	d.Scan(context.Background())
	if reg.players["speaker"].Online {
		t.Fatal("closed control endpoint remains online")
	}
}
