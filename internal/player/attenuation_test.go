package player

import (
	"context"
	"encoding/binary"
	"errors"
	"log/slog"
	"math"
	"songloft/internal/player/dsp"
	"songloft/internal/player/sink"
	"strings"
	"testing"
)

func TestIndependentAttenuationWithoutEQ(t *testing.T) {
	p := &Player{ID: "study", EQ: ParseEQ(`{"enabled":false,"bands":[6,6,6,6,6,6,6,6,6,6],"attenuation_db":6}`)}
	if p.EQ.Flat() {
		t.Fatal("manual attenuation must remain active when EQ is disabled")
	}
	settings := eqSettingsFor(p, sink.EQAppliedByClient)
	if !settings.Enabled || settings.Preamp != -6 {
		t.Fatalf("effective settings: %+v", settings)
	}
	for _, b := range settings.Bands {
		if b != 0 {
			t.Fatal("disabled EQ must not color the sound")
		}
	}
	m := &Manager{eqChains: map[string]*dsp.EQ{}}
	chain := m.eqChainFor(p, 44100)
	pcm := make([]byte, 4096)
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], 10000)
	}
	chain.ProcessInPlace(pcm)
	want := 10000 * math.Pow(10, -6.0/20)
	for i := 0; i < len(pcm); i += 2 {
		if math.Abs(float64(int16(binary.LittleEndian.Uint16(pcm[i:])))-want) > 1 {
			t.Fatalf("sample %d: amplitude does not match -6dB", i)
		}
	}
	// A second speaker in the same group must remain untouched.
	other := m.eqChainFor(&Player{ID: "other"}, 44100)
	if !other.(*dsp.EQ).Flat() {
		t.Fatal("attenuation leaked into another speaker")
	}
	restored := ParseEQ(EQJSON(p.EQ))
	if restored.Flat() {
		t.Fatal("attenuation lost after JSON persistence")
	}
}

func TestSetEQAttenuationNormalizesAndCombinesHeadroom(t *testing.T) {
	m := &Manager{players: map[string]*Player{"study": {ID: "study", Kind: KindRAOP}}, groups: map[string]*Group{}, deps: ManagerDeps{Logger: slog.Default()}}
	for _, tc := range []struct {
		raw string
		pre float64
	}{
		{`{"enabled":true,"bands":[6],"attenuation_db":6}`, -12.1},
		{`{"enabled":false,"bands":[12],"attenuation_db":6}`, -6},
		{`{"enabled":true,"bands":[0],"attenuation_db":99}`, -24},
		{`{"enabled":true,"bands":[0],"attenuation_db":-6}`, 0},
	} {
		got, err := m.SetEQ(context.Background(), "study", ParseEQ(tc.raw))
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(got.Preamp-tc.pre) > 1e-8 {
			t.Fatalf("%s: preamp=%v, want %v", tc.raw, got.Preamp, tc.pre)
		}
	}
}

func TestAttenuationSaveDoesNotChangeHardwareVolume(t *testing.T) {
	p := &Player{ID: "study", Kind: KindRAOP, Volume: 2}
	m := &Manager{players: map[string]*Player{"study": p}, groups: map[string]*Group{}, deps: ManagerDeps{Logger: slog.Default()}}
	input := ParseEQ(`{"enabled":false,"attenuation_db":6}`)
	for i := 0; i < 3; i++ {
		got, err := m.SetEQ(context.Background(), "study", input)
		if err != nil {
			t.Fatal(err)
		}
		if got.Preamp != -6 {
			t.Fatalf("attenuation compounded: %v", got.Preamp)
		}
		input = got
	}
	if p.Volume != 2 {
		t.Fatal("hardware volume changed")
	}
}

type attenuationFailStore struct{ Store }

func (attenuationFailStore) UpdatePlayerEQ(context.Context, Player) error {
	return errors.New("test: disk full")
}
func TestAttenuationFailedSaveLeavesMemoryUnchanged(t *testing.T) {
	p := &Player{ID: "study", Kind: KindRAOP, EQ: ParseEQ(`{"attenuation_db":3}`)}
	m := &Manager{players: map[string]*Player{"study": p}, deps: ManagerDeps{Store: attenuationFailStore{}}}
	if _, err := m.SetEQ(context.Background(), "study", ParseEQ(`{"attenuation_db":6}`)); err == nil {
		t.Fatal("failed persistence reported success")
	}
	if p.EQ.AttenuationDB != 3 {
		t.Fatal("failed save changed live settings")
	}
}
func TestAttenuationSignalPathAndFullRangeGain(t *testing.T) {
	p := &Player{ID: "study", Name: "书房", Kind: KindRAOP, EQ: ParseEQ(`{"enabled":true,"bands":[12],"attenuation_db":24}`)}
	path := buildSignalPath([]*Player{p}, Format{Codec: CodecL16, SampleRate: 44100}, "FLAC", false)
	if path.Kind != SignalKindEnhanced || !strings.Contains(path.Stages[2].Detail, "书房 −24 dB") {
		t.Fatalf("missing attenuation in signal path: %+v", path)
	}
	eq := p.EQ.effective()
	if math.Abs(eq.Preamp-(-36.1)) > 1e-8 {
		t.Fatal(eq.Preamp)
	}
	// High EQ headroom plus manual gain must not silently clamp at the old -24dB limit.
	chain := dsp.NewEQ(44100, 2, make([]float64, 10), eq.Preamp)
	pcm := make([]byte, 4)
	binary.LittleEndian.PutUint16(pcm, 10000)
	chain.ProcessInPlace(pcm)
	if math.Abs(float64(int16(binary.LittleEndian.Uint16(pcm)))-10000*math.Pow(10, -36.1/20)) > 1 {
		t.Fatal("combined gain was clamped")
	}
}

func TestAttenuationAloneIsNotReportedAsCustomEQ(t *testing.T) {
	p := &Player{ID: "study", Name: "书房", Kind: KindRAOP, EQ: ParseEQ(`{"enabled":true,"preset":"flat","bands":[0],"attenuation_db":6}`)}
	path := buildSignalPath([]*Player{p}, Format{Codec: CodecL16, SampleRate: 44100}, "FLAC", false)
	if path.Stages[2].Detail != "前级衰减: 书房 −6 dB" {
		t.Fatal(path.Stages[2].Detail)
	}
}
