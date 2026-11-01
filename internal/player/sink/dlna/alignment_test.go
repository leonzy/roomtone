package dlna

import (
	"context"
	"io"
	"net/http"
	"songloft/internal/player/sink"
	"strings"
	"testing"
	"time"
)

type seekCaptureTransport struct{ targets chan string }

func (tr *seekCaptureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.Header.Get("SOAPAction"), "#Seek") && r.GetBody != nil {
		body, err := r.GetBody()
		if err == nil {
			data, _ := io.ReadAll(body)
			body.Close()
			values, _ := ParseEnvelope(data)
			tr.targets <- values["Target"]
		}
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestMixedS12StartCompensatesReadinessAndPreservesSubseconds(t *testing.T) {
	for _, position := range []time.Duration{0, 30 * time.Second} {
		f := newFakeRenderer(t)
		dev := f.device(t)
		dev.ModelName = "S12"
		tr := &seekCaptureTransport{targets: make(chan string, 2)}
		s := New("mixed", dev, &Client{HC: &http.Client{Transport: tr}}, Options{PollInterval: time.Hour})
		if err := s.Open(context.Background(), sink.StreamPlan{StreamURL: "http://example.invalid/song.flac", Position: position, AlignStart: true}); err != nil {
			t.Fatal(err)
		}
		if err := s.Start(time.Now().Add(-1250 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		select {
		case target := <-tr.targets:
			actual := parseDuration(target)
			if actual < position+1200*time.Millisecond || actual > position+2*time.Second {
				t.Fatalf("start delay lost: target %s, original %v", target, position)
			}
			if !strings.Contains(target, ".") {
				t.Fatal("fractional seek was rounded away")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("mixed start did not align after Play readiness")
		}
		s.Close()
	}
}

func TestAlignmentCannotSeekDifferentSongOrUnreadySession(t *testing.T) {
	f := newFakeRenderer(t)
	s := New("guard", f.device(t), nil, Options{})
	defer s.Close()
	s.plan.SongID = 1
	s.started = true
	s.playing = true
	if err := s.AlignPlayback(context.Background(), 1, time.Second); err == nil {
		t.Fatal("unready session accepted correction")
	}
	s.ready = true
	if err := s.AlignPlayback(context.Background(), 2, time.Second); err == nil {
		t.Fatal("old correction affected new song")
	}
	if len(f.calls()) != 0 {
		t.Fatal("guard still sent SOAP")
	}
}
