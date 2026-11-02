package player_test

import (
	"context"
	"encoding/binary"
	"io"
	"songloft/internal/player"
	"songloft/internal/player/pipeline"
	"songloft/internal/player/sink"
	"songloft/internal/player/sink/fake"
	"strings"
	"sync"
	"testing"
	"time"
)

type surroundResolver struct{}

func (surroundResolver) ResolveTrack(context.Context, int64) (player.Track, error) {
	return player.Track{SongID: 1, Path: "synthetic", Duration: 20 * time.Second}, nil
}

type busDecoder struct {
	mu      sync.Mutex
	formats []int
}
type busReader struct{ io.Reader }

func (busReader) Close() error { return nil }
func (busReader) Err() error   { return nil }
func (b *busDecoder) Open(_ context.Context, _ player.Track, _ time.Duration, f player.Format) (player.Decoder, error) {
	b.mu.Lock()
	b.formats = append(b.formats, f.Channels)
	b.mu.Unlock()
	frame := make([]byte, f.Channels*2)
	for i := 0; i < f.Channels; i++ {
		binary.LittleEndian.PutUint16(frame[i*2:], uint16((i+1)*100))
	}
	return busReader{strings.NewReader(strings.Repeat(string(frame), 44100*20))}, nil
}
func (b *busDecoder) last() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.formats) == 0 {
		return 0
	}
	return b.formats[len(b.formats)-1]
}

type channelSink struct {
	*fake.Sink
	mu     sync.Mutex
	left   int16
	frames int
}

func (s *channelSink) WriteFrame(f sink.Frame) error {
	s.mu.Lock()
	s.left = int16(binary.LittleEndian.Uint16(f.Data))
	s.frames++
	s.mu.Unlock()
	return s.Sink.WriteFrame(f)
}
func (s *channelSink) value() int16 { s.mu.Lock(); defer s.mu.Unlock(); return s.left }

func TestEngineSurroundRoutingSwitchRetainsPosition(t *testing.T) {
	ctx := context.Background()
	decoder := &busDecoder{}
	a := &channelSink{Sink: fake.New("a")}
	b := &channelSink{Sink: fake.New("b")}
	e := player.NewEngine("group", "g", player.EngineDeps{Decoder: decoder, Resolver: surroundResolver{}, Latency: 50 * time.Millisecond, Runners: func(timeline *player.Timeline, targets *sink.Set, start int64, progress func(int64, time.Time)) player.FrameRunner {
		return &pipeline.Distributor{Rate: 44100, Targets: targets, Timeline: timeline, StartSample: start, OnProgress: progress}
	}})
	defer e.Stop()
	targets := []sink.Target{{PlayerID: "a", Sink: a}, {PlayerID: "b", Sink: b}}
	if err := e.SetTargets(ctx, targets); err != nil {
		t.Fatal(err)
	}
	if err := e.Load(ctx, []int64{1}, 0, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := e.Play(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "stereo", 3*time.Second, func() bool { return a.value() == 100 && decoder.last() == 2 })
	targets[0].ChannelMode = "center"
	targets[1].ChannelMode = "side_right"
	if err := e.SetTargets(ctx, targets); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "discrete channels", 3*time.Second, func() bool {
		return decoder.last() == player.SurroundPCMChannels && a.value() == 300 && b.value() == 900
	})
	if status := e.Status(); status.Position < 3 {
		t.Fatalf("lost position on channel change: %+v", status)
	}
	if a.Plan().Format.Channels != 2 {
		t.Fatal("internal bus leaked into speaker format")
	}
	targets[0].ChannelMode = "stereo"
	targets[1].ChannelMode = "stereo"
	if err := e.SetTargets(ctx, targets); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "back to stereo", 3*time.Second, func() bool { return decoder.last() == 2 && a.value() == 100 })
}
