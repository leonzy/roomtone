package player

import (
	"context"
	"io"
	"testing"
	"time"

	"songloft/internal/player/sink"
	"songloft/internal/player/sink/fake"
)

func TestSleepAfterSongsNaturalCompletion(t *testing.T) {
	for _, tc := range []struct{ delegated, repeat bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		delegated := tc.delegated
		name := map[bool]string{false: "push", true: "delegated"}[delegated]
		if tc.repeat {
			name += "/repeat-one"
		}
		t.Run(name, func(t *testing.T) {
			m := NewManager(ManagerDeps{Clock: func() time.Time { return time.Now().Add(-10 * time.Second) }, Resolver: staticResolver{1: {SongID: 1, Duration: time.Millisecond}, 2: {SongID: 2, Duration: time.Millisecond}, 3: {SongID: 3, Duration: time.Millisecond}}, Decoder: completionDecoderFactory{}, Runners: func(*Timeline, *sink.Set, int64, func(int64, time.Time)) FrameRunner {
				return completionRunner{func(context.Context) error { return io.EOF }}
			}})
			m.groups["g"] = &Group{ID: "g"}
			m.ensureEngine(context.Background(), *m.groups["g"])
			e := m.engine("group", "g")
			if err := m.SetSleepAfterSongs("g", 2); err != nil {
				t.Fatal(err)
			}
			cs := newCtrlSink("web")
			cs.setPos(time.Second)
			if delegated {
				e.SetControls([]sink.Sink{cs})
			} else {
				if err := e.SetTargets(context.Background(), []sink.Target{{PlayerID: "a", Sink: fake.New("a")}}); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e.runCtx = ctx
			e.cancel = cancel
			e.running = true
			e.delegated = delegated
			e.queue.Replace([]int64{1, 2, 3}, 0)
			if tc.repeat {
				e.queue.SetRepeat(RepeatOne)
			}
			e.run(ctx, 0)
			st := e.Status()
			wantSong, wantIndex := int64(2), 1
			if tc.repeat {
				wantSong, wantIndex = 1, 0
			}
			if st.State != StatePaused || st.SongID != wantSong || st.Queue.Current != wantIndex || len(st.Queue.Items) != 3 {
				t.Fatalf("sleep did not preserve queue and next entry: %+v", st)
			}
			if _, active := m.SleepSongs("g"); active {
				t.Fatal("completed sleep still active")
			}
			if delegated {
				cs.mu.Lock()
				defer cs.mu.Unlock()
				if len(cs.opened) != 2 {
					t.Fatalf("loaded %d tracks; must not load third", len(cs.opened))
				}
			}
		})
	}
}

func TestSleepAfterSongsReplacementAndCancelDuringDrain(t *testing.T) {
	m := NewManager(ManagerDeps{})
	m.groups["g"] = &Group{ID: "g"}
	for _, replace := range []bool{false, true} {
		if err := m.SetSleepAfterSongs("g", 1); err != nil {
			t.Fatal(err)
		}
		done := make(chan bool, 1)
		go func() { done <- m.completeSleepSong(context.Background(), "g", time.Now().Add(80*time.Millisecond)) }()
		for deadline := time.Now().Add(time.Second); ; {
			n, _ := m.SleepSongs("g")
			if n == 0 {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("no completion")
			}
			time.Sleep(time.Millisecond)
		}
		if replace {
			if err := m.SetSleepAfterSongs("g", 3); err != nil {
				t.Fatal(err)
			}
		} else {
			if err := m.SetSleepTimer("g", 0); err != nil {
				t.Fatal(err)
			}
		}
		if <-done {
			t.Fatal("superseded sleep fired")
		}
	}
	if err := m.SetSleepTimer("g", time.Hour); err != nil {
		t.Fatal(err)
	}
	defer m.SetSleepTimer("g", 0)
	if _, active := m.SleepSongs("g"); active {
		t.Fatal("time timer did not replace count timer")
	}
	if err := m.SetSleepAfterSongs("g", 1); err != nil {
		t.Fatal(err)
	}
	if _, active := m.SleepTimer("g"); active {
		t.Fatal("count timer did not replace time timer")
	}
	if m.completeSleepSong(context.Background(), "other", time.Now()) {
		t.Fatal("another zone triggered sleep")
	}
}

func TestSleepAfterSongsSuppressesLateCrossfade(t *testing.T) {
	e := NewEngine("group", "g", EngineDeps{Decoder: completionDecoderFactory{}, SleepSongsRemaining: func() int { return 1 }})
	if _, err := e.openCrossfadeNext(context.Background(), 2, Format{}); err == nil {
		t.Fatal("next song must not be mixed while sleeping after current")
	}
}

func TestSleepAfterSongsLocalReportsAreGenerationScoped(t *testing.T) {
	m := NewManager(ManagerDeps{})
	m.groups["g"] = &Group{ID: "g", Members: []Member{{PlayerID: "web"}}}
	m.ensureEngine(context.Background(), *m.groups["g"])
	if err := m.SetSleepAfterSongs("g", 3); err != nil {
		t.Fatal(err)
	}
	gen := m.sleepSongs["g"].generation
	two := 2
	m.ReportSleepSongs("web", gen, &two)
	if n, _ := m.SleepSongs("g"); n != 2 {
		t.Fatal(n)
	}
	three := 3
	m.ReportSleepSongs("web", gen, &three)
	if n, _ := m.SleepSongs("g"); n != 2 {
		t.Fatal("older report increased count")
	}
	if err := m.SetSleepAfterSongs("g", 5); err != nil {
		t.Fatal(err)
	}
	zero := 0
	m.ReportSleepSongs("web", gen, &zero)
	if n, _ := m.SleepSongs("g"); n != 5 {
		t.Fatal("old generation cleared replacement")
	}
	gen = m.sleepSongs["g"].generation
	e := m.engine("group", "g")
	e.running = true
	m.ReportSleepSongs("web", gen, &zero)
	if n, _ := m.SleepSongs("g"); n != 5 {
		t.Fatal("delegated report double-counted")
	}
	e.running = false
	m.ReportSleepSongs("web", gen, &zero)
	if _, active := m.SleepSongs("g"); active {
		t.Fatal("finished local sleep remains active")
	}
}
