package player

import (
	"context"
	"io"
	"songloft/internal/player/sink"
	"songloft/internal/player/sink/fake"
	"strings"
	"sync"
	"testing"
	"time"
)

type outputModeDecoder struct {
	mu        sync.Mutex
	positions []time.Duration
}

func (d *outputModeDecoder) Open(_ context.Context, _ Track, position time.Duration, _ Format) (Decoder, error) {
	d.mu.Lock()
	d.positions = append(d.positions, position)
	d.mu.Unlock()
	return completionDecoder{io.NopCloser(strings.NewReader("\x01\x00\x01\x00"))}, nil
}
func (d *outputModeDecoder) snapshot() []time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]time.Duration(nil), d.positions...)
}

type outputModeRunner struct {
	targets  *sink.Set
	start    int64
	progress func(int64, time.Time)
}

func (r outputModeRunner) Run(ctx context.Context, reader io.Reader) (int, error) {
	pcm := make([]byte, 4)
	if _, err := io.ReadFull(reader, pcm); err != nil {
		return 0, err
	}
	targets, _ := r.targets.Snapshot()
	for _, target := range targets {
		if err := target.Sink.WriteFrame(sink.Frame{SampleIndex: r.start, Data: pcm, PlayAt: time.Now().Add(2 * time.Second)}); err != nil {
			return 0, err
		}
	}
	r.progress(r.start, time.Now().Add(2*time.Second))
	<-ctx.Done()
	return 1, ctx.Err()
}

func TestDelegatedPlaybackStartsPCMWhenPushMemberJoins(t *testing.T) {
	decoder := &outputModeDecoder{}
	budget := NewBudget(1)
	e := NewEngine("group", "mixed", EngineDeps{Resolver: staticResolver{1: {SongID: 1, Duration: time.Minute}}, Decoder: decoder,
		Budget: budget,
		Runners: func(_ *Timeline, targets *sink.Set, start int64, progress func(int64, time.Time)) FrameRunner {
			return outputModeRunner{targets, start, progress}
		},
	})
	defer e.Close()
	pull := newCtrlSink("xiaomi")
	if err := e.SetControls([]sink.Sink{pull}); err != nil {
		t.Fatal(err)
	}
	if err := e.Load(context.Background(), []int64{1}, 0, 7*time.Second); err != nil {
		t.Fatal(err)
	}
	waitState(t, e, StatePlaying)
	push := fake.New("study")
	if err := e.SetTargets(context.Background(), []sink.Target{{PlayerID: "study", Sink: push}}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(push.Frames()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(push.Frames()) == 0 {
		t.Fatalf("push member got Open/Start but no PCM; decoder positions=%v", decoder.snapshot())
	}
	positions := decoder.snapshot()
	if len(positions) != 1 || positions[0] < 7*time.Second || positions[0] > 9*time.Second {
		t.Fatalf("join lost position: %v", positions)
	}
	if err := e.SetTargets(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for {
		if plan := pull.lastPlan(); plan.Position >= 7*time.Second {
			opens, _, _, _ := pull.snapshot()
			if opens >= 3 && e.Status().State == StatePlaying {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("removing last push failed to return to delegated mode: %+v", e.Status())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if e.Status().SongID != 1 || e.Status().Position < 7 {
		t.Fatal("mode change lost current song/position")
	}
	if budget.Used() != 1 {
		t.Fatalf("mode change leaked/released budget: %d", budget.Used())
	}
	e.Close()
	if budget.Used() != 0 {
		t.Fatal("closing changed mode leaked decoder budget")
	}
}

type initialVolumeBuilder struct{ output *fake.Sink }

func (b *initialVolumeBuilder) Build(_ context.Context, p *Player) (sink.Sink, error) {
	b.output = fake.New(p.ID)
	_ = b.output.SetVolume(.9)
	return b.output, nil
}

func TestDLNAReconnectAppliesPersistedVolume(t *testing.T) {
	for _, tc := range []struct {
		name        string
		volume, max int
		muted       bool
		want        float64
	}{
		{"stored volume", 37, 100, false, .37}, {"muted", 37, 100, true, 0}, {"limit", 80, 25, false, .25},
	} {
		t.Run(tc.name, func(t *testing.T) {
			builder := &initialVolumeBuilder{}
			m := NewManager(ManagerDeps{Builder: builder})
			if err := m.RegisterPlayer(context.Background(), Player{ID: "dlna", Kind: KindDLNA, Name: "DLNA", Volume: tc.volume, MaxVolume: tc.max, Muted: tc.muted}); err != nil {
				t.Fatal(err)
			}
			if _, err := m.Reconnect(context.Background(), "dlna"); err != nil {
				t.Fatal(err)
			}
			defer builder.output.Close()
			if got := builder.output.Volume(); got != tc.want {
				t.Fatalf("reconnect volume %v want %v", got, tc.want)
			}
		})
	}
}
