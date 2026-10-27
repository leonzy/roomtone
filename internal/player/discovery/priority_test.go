package discovery

import (
	"context"
	"songloft/internal/player"
	"testing"
)

func TestAirPlayPreferredOverDLNA(t *testing.T) {
	reg := newFakeRegistry()
	d := New(Options{Registry: reg, Protocols: []string{"raop", "dlna"}, Scanners: []Scanner{
		&fakeScanner{kind: player.KindRAOP, list: []Speaker{{ID: "airplay", Kind: player.KindRAOP, Address: "192.168.1.106:4515"}}},
		&fakeScanner{kind: player.KindDLNA, list: []Speaker{{ID: "duplicate", Kind: player.KindDLNA, Address: "http://192.168.1.106:49152/description.xml"}, {ID: "dlna-only", Kind: player.KindDLNA, Address: "http://192.168.1.130:49152/description.xml"}}},
	}})
	result, err := d.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Speakers) != 2 || reg.has("duplicate") || !reg.has("airplay") || !reg.has("dlna-only") {
		t.Fatalf("unexpected result: %+v", result)
	}
}
