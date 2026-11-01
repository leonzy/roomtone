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
