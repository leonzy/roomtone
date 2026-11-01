package dlna

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"songloft/internal/player/sink"
)

type transientPlayTransport struct{ attempts atomic.Int32 }

func (tr *transientPlayTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.Header.Get("SOAPAction"), "#Play\"") && tr.attempts.Add(1) == 1 {
		body := `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault><detail><UPnPError><errorCode>701</errorCode><errorDescription>Transition not available</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`
		return &http.Response{StatusCode: 500, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestPlayRetriesTransientRendererState(t *testing.T) {
	f := newFakeRenderer(t)
	tr := &transientPlayTransport{}
	s := New("retry", f.device(t), &Client{HC: &http.Client{Transport: tr}}, Options{PollInterval: time.Hour})
	defer s.Close()
	if err := s.Open(context.Background(), sink.StreamPlan{StreamURL: "http://example.invalid/test.mp3"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(time.Now()); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-s.Events():
			if event.Type == sink.EventError {
				t.Fatal(event.Err)
			}
			if event.Type == sink.EventPlaying {
				if tr.attempts.Load() != 2 {
					t.Fatal("missing retry")
				}
				return
			}
		case <-deadline:
			t.Fatal("renderer never started")
		}
	}
}

func TestStopCancelsScheduledPlay(t *testing.T) {
	f := newFakeRenderer(t)
	s := New("cancel", f.device(t), nil, Options{PollInterval: time.Hour})
	defer s.Close()
	if err := s.Open(context.Background(), sink.StreamPlan{StreamURL: "http://example.invalid/test.mp3"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(time.Now().Add(200 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(350 * time.Millisecond)
	for _, action := range f.calls() {
		if action == "Play" {
			t.Fatal("stopped scheduled playback restarted")
		}
	}
}

func TestPlaybackPositionAdvancesBetweenRendererReports(t *testing.T) {
	f := newFakeRenderer(t)
	s := New("position", f.device(t), nil, Options{})
	defer s.Close()
	s.mu.Lock()
	s.started, s.playing, s.ready = true, true, true
	s.position, s.positionAt = 10*time.Second, time.Now().Add(-2*time.Second)
	s.plan.Duration = time.Minute
	s.mu.Unlock()
	position, valid := s.PlaybackPosition()
	if !valid || position < 12*time.Second || position > 12100*time.Millisecond {
		t.Fatalf("stale position reused between polls: %v %v", position, valid)
	}
	s.onTransportState("PAUSED_PLAYBACK")
	if _, valid := s.PlaybackPosition(); valid {
		t.Fatal("paused position is still extrapolated")
	}
}

func TestRendererReloadSupportsSeekAndNextTrack(t *testing.T) {
	f := newFakeRenderer(t)
	s := New("reload", f.device(t), nil, Options{PollInterval: time.Hour})
	defer s.Close()
	ctx := context.Background()
	for _, plan := range []sink.StreamPlan{
		{SongID: 1, StreamURL: "http://example.invalid/1.flac"},
		{SongID: 1, StreamURL: "http://example.invalid/1.flac", Position: 30 * time.Second},
		{SongID: 2, StreamURL: "http://example.invalid/2.flac"},
	} {
		if err := s.Open(ctx, plan); err != nil {
			t.Fatal(err)
		}
	}
	want := "SetAVTransportURI,Stop,SetAVTransportURI,Seek,Stop,SetAVTransportURI"
	if got := strings.Join(f.calls(), ","); got != want {
		t.Fatalf("reload sequence %s want %s", got, want)
	}
}

func TestXiaomiS12SeeksAfterPlay(t *testing.T) {
	f := newFakeRenderer(t)
	dev := f.device(t)
	dev.ModelName = "S12"
	s := New("s12", dev, nil, Options{PollInterval: time.Hour})
	defer s.Close()
	if err := s.Open(context.Background(), sink.StreamPlan{StreamURL: "http://example.invalid/a.flac", Position: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.calls(), ","); got != "SetAVTransportURI" {
		t.Fatalf("seek before Play resets on S12: %s", got)
	}
	if err := s.Start(time.Now()); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(time.Second)
	for {
		select {
		case event := <-s.Events():
			if event.Type == sink.EventPlaying {
				if got := strings.Join(f.calls(), ","); got != "SetAVTransportURI,Play,GetPositionInfo,Seek" {
					t.Fatal(got)
				}
				return
			}
		case <-deadline:
			t.Fatal("post-Play seek did not finish")
		}
	}
}
