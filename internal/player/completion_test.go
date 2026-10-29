package player

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"songloft/internal/player/sink"
	"songloft/internal/player/sink/fake"
)

type completionDecoder struct{ io.ReadCloser }

func (completionDecoder) Err() error { return nil }

type completionDecoderFactory struct{}

func (completionDecoderFactory) Open(context.Context, Track, time.Duration, Format) (Decoder, error) {
	return completionDecoder{io.NopCloser(strings.NewReader("pcm"))}, nil
}

type completionRunner struct{ run func(context.Context) error }

func (r completionRunner) Run(ctx context.Context, _ io.Reader) (int, error) {
	return 1, r.run(ctx)
}

func TestTrackCompletionPush(t *testing.T) {
	for _, outcome := range []string{"eof", "failure", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			var got []Track
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			e := NewEngine("group", "speakers", EngineDeps{
				Resolver:        staticResolver{1: {SongID: 1, Title: "first", Artist: "artist"}, 2: {SongID: 2}},
				Decoder:         completionDecoderFactory{},
				OnTrackFinished: func(track Track) { got = append(got, track) },
				Runners: func(*Timeline, *sink.Set, int64, func(int64, time.Time)) FrameRunner {
					return completionRunner{func(context.Context) error {
						switch outcome {
						case "failure":
							return errors.New("disconnected")
						case "cancel":
							cancel()
							return context.Canceled
						default:
							return io.EOF
						}
					}}
				},
			})
			if err := e.SetTargets(ctx, []sink.Target{
				{PlayerID: "a", Sink: fake.New("a")},
				{PlayerID: "b", Sink: fake.New("b")},
			}); err != nil {
				t.Fatal(err)
			}
			e.queue.Replace([]int64{1, 2}, 0)
			e.run(ctx, 0)
			if outcome != "eof" {
				if len(got) != 0 {
					t.Fatalf("interrupted playback counted: %+v", got)
				}
				return
			}
			if len(got) != 2 || got[0].SongID != 1 || got[1].SongID != 2 || got[0].Artist != "artist" {
				t.Fatalf("completions must include both songs, including the final one: %+v", got)
			}
		})
	}
}

func TestTrackCompletionDelegated(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "paused"}[cancelEarly], func(t *testing.T) {
			var got []Track
			e := NewEngine("group", "web", EngineDeps{
				Resolver:        staticResolver{1: {SongID: 1, Duration: time.Millisecond}},
				OnTrackFinished: func(track Track) { got = append(got, track) },
			})
			cs := newCtrlSink("web")
			cs.setPos(time.Second)
			e.SetControls([]sink.Sink{cs})
			e.queue.Replace([]int64{1}, 0)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cancelEarly {
				cancel()
			}
			e.runDelegated(ctx, 0)
			want := 1
			if cancelEarly {
				want = 0
			}
			if len(got) != want {
				t.Fatalf("completion count=%d, want %d", len(got), want)
			}
		})
	}
}
