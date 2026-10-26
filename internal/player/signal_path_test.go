package player

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// assertSignalStages 五段名称与顺序必须固定，前端按位置渲染。
func assertSignalStages(t *testing.T, got SignalPath) {
	t.Helper()
	want := []string{"源", "解码", "DSP", "输出", "设备"}
	if len(got.Stages) != len(want) {
		t.Fatalf("stages 数量 = %d，期望 %d：%+v", len(got.Stages), len(want), got.Stages)
	}
	for i, name := range want {
		if got.Stages[i].Name != name {
			t.Fatalf("stages[%d].name = %q，期望 %q", i, got.Stages[i].Name, name)
		}
	}
}

func flatEQ() EQProfile {
	return EQProfile{Bands: make([]float64, EQBandCount)}
}

// 无 EQ、无响度、输出无损 → lossless，DSP 段写"直通"。
func TestSignalPathLossless(t *testing.T) {
	players := []*Player{{ID: "a", Name: "客厅", EQ: flatEQ()}}
	got := buildSignalPath(players,
		Format{Codec: CodecL16, SampleRate: 44100, Channels: 2, Bits: 16},
		"FLAC 44.1kHz", false)

	if got.Kind != SignalKindLossless {
		t.Fatalf("kind = %q，期望 %q", got.Kind, SignalKindLossless)
	}
	assertSignalStages(t, got)
	if got.Stages[0].Detail != "FLAC 44.1kHz" {
		t.Fatalf("源 = %q", got.Stages[0].Detail)
	}
	if got.Stages[2].Detail != "直通" {
		t.Fatalf("DSP = %q，期望 直通", got.Stages[2].Detail)
	}
	if got.Stages[3].Detail != "L16 44.1kHz/16bit" {
		t.Fatalf("输出 = %q", got.Stages[3].Detail)
	}
	if got.Stages[4].Detail != "客厅" {
		t.Fatalf("设备 = %q", got.Stages[4].Detail)
	}
}

// 任一成员 EQ 非 flat → enhanced，DSP 段带预设名。
func TestSignalPathEnhancedEQ(t *testing.T) {
	players := []*Player{{
		ID:   "a",
		Name: "客厅",
		EQ: EQProfile{
			Enabled: true,
			Preset:  "rock",
			Bands:   []float64{3, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		},
	}}
	got := buildSignalPath(players,
		Format{Codec: CodecL16, SampleRate: 44100, Channels: 2, Bits: 16},
		"FLAC 44.1kHz", false)

	if got.Kind != SignalKindEnhanced {
		t.Fatalf("kind = %q，期望 %q", got.Kind, SignalKindEnhanced)
	}
	assertSignalStages(t, got)
	if got.Stages[2].Detail != "EQ: rock" {
		t.Fatalf("DSP = %q，期望 EQ: rock", got.Stages[2].Detail)
	}
}

// 输出 AAC/MP3 → lossy，即使同时开了 EQ 也以有损为准。
func TestSignalPathLossy(t *testing.T) {
	players := []*Player{{
		ID:   "a",
		Name: "客厅",
		EQ: EQProfile{
			Enabled: true,
			Preset:  "rock",
			Bands:   []float64{3, 0, 0, 0, 0, 0, 0, 0, 0, 0},
		},
	}}
	got := buildSignalPath(players,
		Format{Codec: CodecAAC, SampleRate: 44100, Channels: 2, Bits: 16},
		"MP3 44.1kHz 320kbps", false)

	if got.Kind != SignalKindLossy {
		t.Fatalf("kind = %q，期望 %q", got.Kind, SignalKindLossy)
	}
	assertSignalStages(t, got)
	if got.Stages[3].Detail != "AAC 44.1kHz/16bit" {
		t.Fatalf("输出 = %q", got.Stages[3].Detail)
	}
	if got.Stages[2].Detail != "EQ: rock" {
		t.Fatalf("DSP = %q，有损不应吞掉 DSP 信息", got.Stages[2].Detail)
	}
}

// 响度归一单独开启也算 enhanced。
func TestSignalPathEnhancedLoudness(t *testing.T) {
	players := []*Player{{ID: "a", Name: "客厅", EQ: flatEQ()}}
	got := buildSignalPath(players,
		Format{Codec: CodecL16, SampleRate: 44100, Channels: 2, Bits: 16},
		"FLAC 44.1kHz", true)

	if got.Kind != SignalKindEnhanced {
		t.Fatalf("kind = %q，期望 %q", got.Kind, SignalKindEnhanced)
	}
	if got.Stages[2].Detail != "响度归一" {
		t.Fatalf("DSP = %q", got.Stages[2].Detail)
	}
}

// 源格式未知时必须写 unknown，不能拿扩展名或输出格式顶替。
func TestSourceFormatDetail(t *testing.T) {
	cases := []struct {
		name  string
		track Track
		want  string
	}{
		{"unknown", Track{}, "unknown"},
		{"flac", Track{Format: "flac", SampleRate: 44100}, "FLAC 44.1kHz"},
		{"mp3", Track{Format: "mp3", SampleRate: 44100, BitRate: 320}, "MP3 44.1kHz 320kbps"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sourceFormatDetail(tc.track); got != tc.want {
				t.Fatalf("sourceFormatDetail = %q，期望 %q", got, tc.want)
			}
		})
	}
}

// GroupView 必须自动带出 signal_path；开 EQ 后同一区域从 lossless 变 enhanced。
func TestGroupViewSignalPathEQTransition(t *testing.T) {
	p := &Player{
		ID:   "a",
		Name: "客厅",
		EQ:   flatEQ(),
		Caps: Caps{Codecs: []Codec{CodecL16}, MaxSampleRate: 44100, Channels: 2, BitDepth: 16},
	}
	g := &Group{ID: "g", Name: "客厅", Members: []Member{{PlayerID: "a"}}}
	e := NewEngine("group", "g", EngineDeps{Logger: slog.Default()})
	e.SetFormat(Format{Codec: CodecL16, SampleRate: 44100, Channels: 2, Bits: 16})
	e.sourceFormat = "FLAC 44.1kHz"

	m := &Manager{
		deps:        ManagerDeps{Logger: slog.Default()},
		players:     map[string]*Player{"a": p},
		groups:      map[string]*Group{"g": g},
		engines:     map[string]*Engine{"group:g": e},
		syncPending: map[string]time.Time{},
	}

	views := m.ListGroups()
	if len(views) != 1 {
		t.Fatalf("ListGroups 返回 %d 个区域，期望 1", len(views))
	}
	view := views[0]
	if view.SignalPath.Kind != SignalKindLossless {
		t.Fatalf("开 EQ 前 kind = %q，期望 %q", view.SignalPath.Kind, SignalKindLossless)
	}
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"signal_path"`) {
		t.Fatalf("JSON 缺少 signal_path：%s", data)
	}

	if _, err := m.SetEQ(context.Background(), "a", EQProfile{
		Enabled: true,
		Preset:  "rock",
		Bands:   []float64{3, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	}); err != nil {
		t.Fatalf("SetEQ: %v", err)
	}
	views = m.ListGroups()
	view = views[0]
	if view.SignalPath.Kind != SignalKindEnhanced {
		t.Fatalf("开 EQ 后 kind = %q，期望 %q", view.SignalPath.Kind, SignalKindEnhanced)
	}
	if view.SignalPath.Stages[2].Detail != "EQ: rock" {
		t.Fatalf("DSP = %q", view.SignalPath.Stages[2].Detail)
	}
}

// 输出协商成 AAC 时 GroupView 必须标 lossy。
func TestGroupViewSignalPathLossy(t *testing.T) {
	p := &Player{ID: "a", Name: "客厅", EQ: flatEQ()}
	g := &Group{ID: "g", Name: "客厅", Members: []Member{{PlayerID: "a"}}}
	e := NewEngine("group", "g", EngineDeps{Logger: slog.Default()})
	e.SetFormat(Format{Codec: CodecAAC, SampleRate: 44100, Channels: 2, Bits: 16})

	m := &Manager{
		deps:        ManagerDeps{Logger: slog.Default()},
		players:     map[string]*Player{"a": p},
		groups:      map[string]*Group{"g": g},
		engines:     map[string]*Engine{"group:g": e},
		syncPending: map[string]time.Time{},
	}

	view := m.groupViewLocked(g)
	if view.SignalPath.Kind != SignalKindLossy {
		t.Fatalf("kind = %q，期望 %q", view.SignalPath.Kind, SignalKindLossy)
	}
}

// loudnessStub 只实现可选接口，用来验证 ReplayGain 开启时 GroupView 标 enhanced。
type loudnessStub struct{ enabled bool }

func (loudnessStub) GainDB(context.Context, int64) (float64, bool) { return 0, false }
func (l loudnessStub) LoudnessEnabled() bool                       { return l.enabled }

func TestGroupViewSignalPathLoudness(t *testing.T) {
	p := &Player{ID: "a", Name: "客厅", EQ: flatEQ()}
	g := &Group{ID: "g", Name: "客厅", Members: []Member{{PlayerID: "a"}}}
	e := NewEngine("group", "g", EngineDeps{Logger: slog.Default()})
	e.SetFormat(Format{Codec: CodecL16, SampleRate: 44100, Channels: 2, Bits: 16})

	m := &Manager{
		deps:        ManagerDeps{Logger: slog.Default(), ReplayGain: loudnessStub{enabled: true}},
		players:     map[string]*Player{"a": p},
		groups:      map[string]*Group{"g": g},
		engines:     map[string]*Engine{"group:g": e},
		syncPending: map[string]time.Time{},
	}

	view := m.groupViewLocked(g)
	if view.SignalPath.Kind != SignalKindEnhanced {
		t.Fatalf("kind = %q，期望 %q", view.SignalPath.Kind, SignalKindEnhanced)
	}
	if view.SignalPath.Stages[2].Detail != "响度归一" {
		t.Fatalf("DSP = %q", view.SignalPath.Stages[2].Detail)
	}
}
