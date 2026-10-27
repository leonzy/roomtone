package raop2

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"songloft/internal/player/sink"
	"testing"
	"time"
)

func loopbackUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func readDatagram(t *testing.T, c *net.UDPConn) []byte {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 2048)
	n, _, err := c.ReadFromUDP(b)
	if err != nil {
		t.Fatal(err)
	}
	return b[:n]
}

func TestTimingRepliesEchoRequestTransmitAndSequence(t *testing.T) {
	s := New("v2", Options{Address: "127.0.0.1:7000"})
	s.timng = loopbackUDP(t)
	peer := loopbackUDP(t)
	s.startTimingReader()
	// The timing reader must reply repeatedly even while Open holds the sink lock.
	s.mu.Lock()
	defer s.mu.Unlock()
	for seq := uint16(71); seq < 74; seq++ {
		req := make([]byte, 32)
		req[0], req[1] = 0x80, 0xd2
		binary.BigEndian.PutUint16(req[2:4], seq)
		copy(req[24:], []byte{0xe1, 2, 3, 4, 5, 6, 7, 8})
		if _, err := peer.WriteToUDP(req, s.timng.LocalAddr().(*net.UDPAddr)); err != nil {
			t.Fatal(err)
		}
		got := readDatagram(t, peer)
		if len(got) != 32 || got[1] != 0xd3 || !bytes.Equal(got[2:4], req[2:4]) || !bytes.Equal(got[8:16], req[24:32]) || binary.BigEndian.Uint64(got[16:24]) == 0 || binary.BigEndian.Uint64(got[24:32]) == 0 {
			t.Fatalf("invalid NTP timing reply: %x", got)
		}
	}
}

func TestRetransmitRepairsContiguousRangeAcrossSequenceWrap(t *testing.T) {
	s := New("v2", Options{Address: "127.0.0.1:7000"})
	s.ctrl = loopbackUDP(t)
	peer := loopbackUDP(t)
	s.backlog = map[uint16][]byte{65535: {1, 2, 3}, 0: {4, 5, 6}, 1: {7, 8, 9}}
	s.startControlReader()
	req := []byte{0x80, 0xd5, 0, 1, 0xff, 0xff, 0, 3}
	if _, err := peer.WriteToUDP(req, s.ctrl.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	for _, seq := range []uint16{65535, 0, 1} {
		got := readDatagram(t, peer)
		if len(got) < 4 || got[0] != 0x80 || got[1] != 0xd6 || binary.BigEndian.Uint16(got[2:4]) != seq || !bytes.Equal(got[4:], s.backlog[seq]) {
			t.Fatalf("wrong resend for %d: %x", seq, got)
		}
	}
}

func TestFirstAudioFrameSendsSyncToControlPort(t *testing.T) {
	audio, control := loopbackUDP(t), loopbackUDP(t)
	s := New("v2", Options{Address: "127.0.0.1:7000"})
	s.audio, s.ctrl = loopbackUDP(t), loopbackUDP(t)
	s.serverPort, s.controlPort = localPort(audio), localPort(control)
	s.backlog = map[uint16][]byte{}
	if err := s.Start(time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteFrame(sink.Frame{Data: make([]byte, 352*4)}); err != nil {
		t.Fatal(err)
	}
	sync := readDatagram(t, control)
	if delta := binary.BigEndian.Uint32(sync[16:20]) - binary.BigEndian.Uint32(sync[4:8]); delta != uint32(s.opts.SampleRate) {
		t.Fatalf("sync latency samples: got %d", delta)
	}
	if len(sync) != 20 || sync[1] != 0xd4 {
		t.Fatalf("wrong sync packet: %x", sync)
	}
	pkt := readDatagram(t, audio)
	if len(pkt) < 12 || pkt[1]&0x7f != 0x60 {
		t.Fatalf("sync sent to audio port: %x", pkt)
	}
}

func TestRTSPTransmitsDigestAuthorization(t *testing.T) {
	d := newFakeV2Device(t)
	c, err := Dial(context.Background(), d.addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.authorization = `Digest username="iTunes", response="test"`
	if _, err = c.Do(context.Background(), "OPTIONS", "*", nil, nil); err != nil {
		t.Fatal(err)
	}
	reqs := d.requests()
	if len(reqs) != 1 || reqs[0].headers["authorization"] != c.authorization {
		t.Fatal("digest header was not sent")
	}
}
