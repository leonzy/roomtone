package raop2

import (
	"bufio"
	"net"
	"testing"
	"time"
)

func TestSlowMetadataDoesNotHoldAudioMutex(t *testing.T) {
	client, peer := net.Pipe()
	defer client.Close()
	defer peer.Close()
	s := New("speaker", Options{Address: "127.0.0.1:7000"})
	s.rtsp = &RTSPClient{conn: client, br: bufio.NewReader(client), timeout: time.Second}
	done := make(chan error, 1)
	go func() { done <- s.UpdateProgress(time.Second, time.Minute) }()
	peer.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 4096)
	if _, err := peer.Read(b); err != nil {
		t.Fatal(err)
	}
	if !s.mu.TryLock() {
		t.Fatal("screen request holds the PCM writer mutex while waiting for a response")
	}
	s.mu.Unlock()
	peer.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("metadata request did not exit after disconnect")
	}
}
