package raop

import (
	"context"
	"encoding/binary"
	"net"
	"songloft/internal/player/sink"
	"sync/atomic"
	"testing"
	"time"
)

func TestHandshakeRetriesSetupAndKeepsAbsoluteOrigin(t *testing.T) {
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	f := newFakeRTSP(t)
	f.dataPort = udp.LocalAddr().(*net.UDPAddr).Port
	var attempts atomic.Int32
	f.onRequest = func(method string, _ map[string]string) int {
		if method == "SETUP" && attempts.Add(1) < 3 {
			return 500
		}
		return 200
	}
	s := New("join", Options{Address: f.addr()})
	defer s.Close()
	sample := int64(44100 * 112)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Open(ctx, sink.StreamPlan{Position: 15 * time.Second, SampleIndex: &sample}); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts=%d", attempts.Load())
	}
	if s.anchor != sample || s.tsLocked(sample) != s.rtpBase {
		t.Fatal("track position was used as absolute PCM origin")
	}
	if err := s.Start(time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteFrame(sink.Frame{SampleIndex: sample, Data: make([]byte, 352*4)}); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2048)
	udp.SetReadDeadline(time.Now().Add(time.Second))
	n, _, err := udp.ReadFromUDP(buf)
	if err != nil || n < 12 {
		t.Fatalf("RTP packet bytes=%d err=%v", n, err)
	}
	if got := binary.BigEndian.Uint32(buf[4:8]); got != s.rtpAtPlay {
		t.Fatalf("first packet timestamp=%d, RECORD/start origin=%d", got, s.rtpAtPlay)
	}

}
func TestHandshakeDoesNotRetryAuthorizationFailure(t *testing.T) {
	f := newFakeRTSP(t)
	var attempts atomic.Int32
	f.onRequest = func(method string, _ map[string]string) int {
		if method == "OPTIONS" {
			attempts.Add(1)
			return 403
		}
		return 200
	}
	s := New("join", Options{Address: f.addr()})
	defer s.Close()
	if err := s.Open(context.Background(), sink.StreamPlan{}); err == nil {
		t.Fatal("authorization failure hidden")
	}
	if attempts.Load() != 1 {
		t.Fatal("authorization failure retried")
	}
}
func TestHandshakeRetryHonorsCancellation(t *testing.T) {
	f := newFakeRTSP(t)
	f.onRequest = func(method string, _ map[string]string) int { return 500 }
	s := New("join", Options{Address: f.addr()})
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := s.Open(ctx, sink.StreamPlan{}); err != context.DeadlineExceeded {
		t.Fatalf("err=%v", err)
	}
}
