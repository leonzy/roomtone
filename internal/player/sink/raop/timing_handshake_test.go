package raop

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"songloft/internal/player/sink"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Some receivers perform several timing exchanges before completing SETUP or
// RECORD. The timing server must keep responding while Open owns s.mu.
func TestTimingRepliesContinueDuringRTSPHandshake(t *testing.T) {
	listen := func() *net.UDPConn {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	server, peer := listen(), listen()
	s := New("timing-handshake", Options{})
	t.Cleanup(func() { s.Close() })
	s.timing = server
	s.startTimingReader(server)
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < 4; i++ {
		req := make([]byte, 32)
		req[0], req[1] = 0x80, 0xd2
		binary.BigEndian.PutUint16(req[2:4], uint16(i+7))
		binary.BigEndian.PutUint64(req[24:], uint64(1234567+i))
		if _, err := peer.WriteToUDP(req, server.LocalAddr().(*net.UDPAddr)); err != nil {
			t.Fatal(err)
		}
		peer.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		reply := make([]byte, 64)
		n, _, err := peer.ReadFromUDP(reply)
		if err != nil {
			t.Fatalf("timing reply %d blocked by handshake lock: %v", i+1, err)
		}
		if n != 32 || reply[1] != 0xd3 || !bytes.Equal(reply[2:4], req[2:4]) || !bytes.Equal(reply[8:16], req[24:32]) {
			t.Fatalf("invalid timing reply: %x", reply[:n])
		}
	}
}

func TestOpenReceiverWaitsForMultipleTimingExchanges(t *testing.T) {
	f := newFakeRTSP(t)
	peer, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	var timingPort int
	var timingErr error
	f.onRequest = func(method string, headers map[string]string) int {
		if method == "SETUP" {
			for _, part := range strings.Split(headers["transport"], ";") {
				if value, ok := strings.CutPrefix(part, "timing_port="); ok {
					timingPort, _ = strconv.Atoi(value)
				}
			}
		}
		if method != "RECORD" {
			return 200
		}
		for i := 0; i < 4; i++ {
			req := make([]byte, 32)
			req[0], req[1] = 0x80, 0xd2
			binary.BigEndian.PutUint64(req[24:], uint64(i+9))
			_, timingErr = peer.WriteToUDP(req, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: timingPort})
			if timingErr != nil {
				return 500
			}
			peer.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
			response := make([]byte, 32)
			_, _, timingErr = peer.ReadFromUDP(response)
			if timingErr != nil {
				return 500
			}
		}
		return 200
	}
	s := New("gated-receiver", Options{Address: f.addr()})
	defer s.Close()
	if err := s.Open(context.Background(), sink.StreamPlan{}); err != nil {
		t.Fatalf("Open failed while receiver was synchronizing: %v; timing=%v", err, timingErr)
	}
}

func TestFailedOpenReleasesAcknowledgedSession(t *testing.T) {
	f := newFakeRTSP(t)
	f.onRequest = func(method string, _ map[string]string) int {
		if method == "RECORD" {
			return 500
		}
		return 200
	}
	s := New("failed-record", Options{Address: f.addr()})
	defer s.Close()
	if err := s.Open(context.Background(), sink.StreamPlan{}); err == nil {
		t.Fatal("accepted failed RECORD")
	}
	methods, _ := f.snapshot()
	if methods[len(methods)-1] != "TEARDOWN" {
		t.Fatalf("failed handshake left receiver session open: %v", methods)
	}
	if s.rtsp != nil || s.timing != nil {
		t.Fatal("failed handshake retained local sockets")
	}
}
