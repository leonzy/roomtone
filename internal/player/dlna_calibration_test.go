package player

import (
	"testing"
	"time"
)

func TestDLNAAutoCompensationPolicy(t *testing.T) {
	cases := []struct {
		name    string
		players []Player
		legacy  int
		want    map[string]int
	}{
		{"solo dlna", []Player{{ID: "d", Kind: KindDLNA, DLNASyncDelayMs: 1000}}, 0, map[string]int{"d": 0}},
		{"solo airplay ignores legacy", []Player{{ID: "a", Kind: KindRAOP}}, 1000, map[string]int{"a": 0}},
		{"exact only ignores legacy", []Player{{ID: "a", Kind: KindRAOP}, {ID: "b", Kind: KindRAOP}}, 1000, map[string]int{"a": 0, "b": 0}},
		{"mixed", []Player{{ID: "d", Kind: KindDLNA, DLNASyncDelayMs: 1000}, {ID: "a", Kind: KindRAOP}, {ID: "b", Kind: KindRAOP}}, 1000, map[string]int{"d": 0, "a": 1000, "b": 1000}},
		{"multiple dlna", []Player{{ID: "slow", Kind: KindDLNA, DLNASyncDelayMs: 1000}, {ID: "fast", Kind: KindDLNA, DLNASyncDelayMs: 600}, {ID: "web", Kind: KindLocal}}, 0, map[string]int{"slow": 0, "fast": 400, "web": 1000}},
		{"default zero", []Player{{ID: "d", Kind: KindDLNA}, {ID: "a", Kind: KindRAOP}}, 0, map[string]int{"d": 0, "a": 0}},
		{"standby not reference", []Player{{ID: "d", Kind: KindDLNA, DLNASyncDelayMs: 1000, Standby: true}, {ID: "a", Kind: KindRAOP}}, 0, map[string]int{"a": 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := NewManager(ManagerDeps{})
			g := &Group{}
			for _, p := range tc.players {
				p := p
				m.players[p.ID] = &p
				g.Members = append(g.Members, Member{PlayerID: p.ID, DelayMs: tc.legacy})
			}
			got := m.groupDelaysLocked(g)
			for id, want := range tc.want {
				if got[id] != time.Duration(want)*time.Millisecond {
					t.Fatalf("%s = %v, want %d ms", id, got[id], want)
				}
			}
		})
	}
}
