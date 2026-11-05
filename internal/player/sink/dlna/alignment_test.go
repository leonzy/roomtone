package dlna

import (
	"context"
	"io"
	"net/http"
	"songloft/internal/player/sink"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type seekCaptureTransport struct {
	targets chan string
	ranges  chan time.Duration
}

func TestDuerOSTransferSeeksAfterPlayReset(t *testing.T) {
	f := newFakeRenderer(t)
	f.resetOnPlay = true
	dev := f.device(t)
	dev.ModelName = "DuerOS-Render"
	s := New("dueros", dev, nil, Options{PollInterval: time.Hour})
	defer s.Close()
	if err := s.Open(context.Background(), sink.StreamPlan{StreamURL: "http://example.invalid/song.flac", Position: 135 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(time.Now()); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case ev := <-s.Events():
			if ev.Type == sink.EventError {
				t.Fatal(ev.Err)
			}
			if ev.Type == sink.EventPlaying {
				s.poll(context.Background())
				position, ok := s.PlaybackPosition()
				if !ok || position < 135*time.Second || position > 136*time.Second {
					t.Fatalf("Play reset the transferred position: %v (%v)", position, ok)
				}
				return
			}
		case <-deadline:
			t.Fatal("transfer did not finish seeking")
		}
	}
}

func TestS12ResumeRequiresByteVerifiableRepresentation(t *testing.T) {
	for _, position := range []time.Duration{0, 135 * time.Second} {
		f := newFakeRenderer(t)
		dev := f.device(t)
		dev.ModelName = "S12"
		dev.Codecs = []sink.Codec{sink.CodecFLAC, sink.CodecWAV}
		var accepted []sink.Codec
		s := New("s12", dev, nil, Options{ResolveStream: func(_ context.Context, plan sink.StreamPlan) (sink.StreamPlan, error) {
			accepted = plan.AcceptedCodecs
			return plan, nil
		}})
		if err := s.Open(context.Background(), sink.StreamPlan{StreamURL: "http://example.invalid/song.flac", Position: position}); err != nil {
			t.Fatal(err)
		}
		s.Close()
		codec := SelectCodec("flac", accepted)
		if position > 0 && codec != sink.CodecWAV {
			t.Fatalf("resume still trusts compressed counter: %s", codec)
		}
		if position == 0 && codec != sink.CodecFLAC {
			t.Fatalf("normal start unnecessarily converted: %s", codec)
		}
	}
}

func (tr *seekCaptureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if strings.Contains(r.Header.Get("SOAPAction"), "#Seek") && r.GetBody != nil {
		body, err := r.GetBody()
		if err == nil {
			data, _ := io.ReadAll(body)
			body.Close()
			values, _ := ParseEnvelope(data)
			tr.targets <- values["Target"]
			if tr.ranges != nil {
				tr.ranges <- parseDuration(values["Target"])
			}
		}
	}
	return http.DefaultTransport.RoundTrip(r)
}

func TestMixedS12StartCompensatesReadinessAndUsesFirmwarePrecision(t *testing.T) {
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
			if actual < position+time.Second || actual > position+2*time.Second {
				t.Fatalf("start delay lost: target %s, original %v", target, position)
			}
			if strings.Contains(target, ".") {
				t.Fatal("S12 received unsupported fractional seek")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("mixed start did not align after Play readiness")
		}
		s.Close()
	}
}

func TestGenericRendererRetainsFractionalSeekPrecision(t *testing.T) {
	if got := formatSeekDuration(1500 * time.Millisecond); got != "0:00:01.500" {
		t.Fatal(got)
	}
}

func TestS12IgnoredSeekDoesNotReportSuccess(t *testing.T) {
	f := newFakeRenderer(t)
	f.ignoreSeek = true
	dev := f.device(t)
	dev.ModelName = "S12"
	s := New("ignored", dev, nil, Options{PollInterval: time.Hour})
	defer s.Close()
	if err := s.Open(context.Background(), sink.StreamPlan{StreamURL: "http://example.invalid/song.flac", Position: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	s.Start(time.Now())
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-s.Events():
			if ev.Type == sink.EventPlaying {
				t.Fatal("acknowledged but ignored seek was reported as ready")
			}
			if ev.Type == sink.EventError {
				if !strings.Contains(ev.Err.Error(), "without applying") {
					t.Fatal(ev.Err)
				}
				return
			}
		case <-deadline:
			t.Fatal("ignored seek did not reach bounded failure")
		}
	}
}

func TestPauseStopsPullPlaybackAndCancelsScheduledStart(t *testing.T) {
	f := newFakeRenderer(t)
	s := New("pause", f.device(t), nil, Options{PollInterval: time.Hour})
	defer s.Close()
	if err := s.Open(context.Background(), sink.StreamPlan{StreamURL: "http://example.invalid/song.flac"}); err != nil {
		t.Fatal(err)
	}
	s.Start(time.Now().Add(200 * time.Millisecond))
	pauser, ok := any(s).(interface{ Pause() error })
	if !ok {
		t.Fatal("DLNA output has no Pause implementation")
	}
	if err := pauser.Pause(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if got := strings.Join(f.calls(), ","); got != "SetAVTransportURI,Pause" {
		t.Fatal("paused renderer restarted", got)
	}
	f.mu.Lock()
	state := f.state
	f.mu.Unlock()
	if state != "PAUSED_PLAYBACK" {
		t.Fatal(state)
	}
	if _, valid := s.PlaybackPosition(); valid {
		t.Fatal("paused renderer is still reported playing")
	}
}

func TestS12WAVUsesRequestedRangeInsteadOfBrokenRelTime(t *testing.T) {
	f := newFakeRenderer(t)
	f.ignoreSeek = true
	dev := f.device(t)
	dev.ModelName = "S12"
	ranges := make(chan time.Duration, 4)
	tr := &seekCaptureTransport{targets: make(chan string, 4), ranges: ranges}
	s := New("wav", dev, &Client{HC: &http.Client{Transport: tr}}, Options{PollInterval: time.Hour})
	defer s.Close()
	if err := s.Open(context.Background(), sink.StreamPlan{SongID: 1, StreamURL: "http://example.invalid/song.wav", Format: sink.Format{Codec: sink.CodecWAV}, Position: 30 * time.Second, Duration: time.Minute, RangeRequests: ranges}); err != nil {
		t.Fatal(err)
	}
	s.Start(time.Now())
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-s.Events():
			if ev.Type == sink.EventError {
				t.Fatal(ev.Err)
			}
			if ev.Type == sink.EventPlaying {
				s.poll(context.Background())
				position, valid := s.PlaybackPosition()
				if !valid || position < 30*time.Second || position > 31*time.Second {
					t.Fatal("broken WAV counter replaced confirmed range", position, valid)
				}
				if s.PlaybackAlignmentAvailable() {
					t.Fatal("unreliable WAV counter enabled feedback correction")
				}
				return
			}
		case <-deadline:
			t.Fatal("range-confirmed WAV playback did not start")
		}
	}
}

func TestStreamObservationReleasedAcrossReloadAndClose(t *testing.T) {
	f := newFakeRenderer(t)
	s := New("release", f.device(t), nil, Options{PollInterval: time.Hour})
	var released atomic.Int32
	for i := 0; i < 2; i++ {
		if err := s.Open(context.Background(), sink.StreamPlan{StreamURL: "http://example.invalid/test.wav", ReleaseStream: func() { released.Add(1) }}); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	if released.Load() != 2 {
		t.Fatalf("observers released %d times, want 2", released.Load())
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
