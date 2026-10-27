package raop

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"songloft/internal/player/sink"
)

// assertDecodedALAC 用 ffmpeg 解 CAF 校验 ALAC 包还原出源 PCM
// （与 airplay/alac_test.go 同实现；测试助手不跨包共享）。
func assertDecodedALAC(t *testing.T, packets [][]byte, pcm []byte) {
	t.Helper()
	ffmpeg := os.Getenv("TEST_FFMPEG")
	if ffmpeg == "" {
		t.Skip("TEST_FFMPEG not configured")
	}
	var caf bytes.Buffer
	caf.Write([]byte{'c', 'a', 'f', 'f', 0, 1, 0, 0})
	chunk := func(name string, data []byte) {
		caf.WriteString(name)
		_ = binary.Write(&caf, binary.BigEndian, int64(len(data)))
		caf.Write(data)
	}
	desc, _ := hex.DecodeString("40e5888000000000616c61630000000000000000000001600000000200000000")
	cookie, _ := hex.DecodeString("0000000c66726d61616c616300000024616c616300000000000001600010280a0e02000000004004001588800000ac44")
	chunk("desc", desc)
	chunk("kuki", cookie)
	data := make([]byte, 4)
	for _, packet := range packets {
		data = append(data, packet...)
	}
	chunk("data", data)
	pakt := make([]byte, 24)
	binary.BigEndian.PutUint64(pakt, uint64(len(packets)))
	binary.BigEndian.PutUint64(pakt[8:], uint64(len(pcm)/4))
	for _, packet := range packets {
		pakt = append(pakt, byte(len(packet)>>7)|0x80, byte(len(packet)&127))
	}
	chunk("pakt", pakt)
	path := filepath.Join(t.TempDir(), "frame.caf")
	if err := os.WriteFile(path, caf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(ffmpeg, "-v", "error", "-i", path, "-f", "s16le", "-")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	decoded, err := cmd.Output()
	if err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	if !bytes.Equal(decoded, pcm) {
		t.Fatalf("decoded PCM differs: got %d bytes, want %d", len(decoded), len(pcm))
	}
}

func datagramSink(t *testing.T, alac bool) (*Sink, *net.UDPConn) {
	t.Helper()
	listen := func() *net.UDPConn {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	receiver := listen()
	s := New("loopback", Options{ALAC: alac})
	s.audio, s.control = listen(), listen()
	s.dataAddr = receiver.LocalAddr().(*net.UDPAddr)
	s.rtpBase = 100000
	s.pkt.Seq = 65534
	s.startAt = time.Now()
	t.Cleanup(func() { s.Close() })
	return s, receiver
}

func readPacket(t *testing.T, conn *net.UDPConn) []byte {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 2048)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	return buf[:n]
}

func TestPacketizationPreservesPCMAndFixed352Samples(t *testing.T) {
	checkPacketizedPCM(t, false)
}

func TestEncryptedALACStreamDecodesWithoutSampleLoss(t *testing.T) {
	checkPacketizedPCM(t, true)
}

func checkPacketizedPCM(t *testing.T, alac bool) {
	s, receiver := datagramSink(t, alac)
	if alac {
		s.enc = true
		s.aesKey = bytes.Repeat([]byte{0x81}, 16)
		s.aesIV = bytes.Repeat([]byte{0x19}, 16)
	}
	var source, received []byte
	var encoded [][]byte
	packets := 0
	// 176 distributor frames are exactly divisible into 441 RAOP packets.
	for frame := 0; frame < 176; frame++ {
		pcm := make([]byte, 882*4)
		for i := range pcm {
			pcm[i] = byte(frame*37 + i*13)
		}
		source = append(source, pcm...)
		if err := s.WriteFrame(sink.Frame{SampleIndex: int64(frame * 882), Data: pcm}); err != nil {
			t.Fatal(err)
		}
		for packets < (frame+1)*882/frameSamples {
			packet := readPacket(t, receiver)
			wantSize := 12 + frameSamples*4
			if alac {
				wantSize += 8
			}
			if len(packet) != wantSize {
				t.Fatalf("short packet %d: %d bytes", packets, len(packet))
			}
			if got := binary.BigEndian.Uint32(packet[4:8]); got != 100000+uint32(packets*frameSamples) {
				t.Fatalf("timestamp gap at %d: %d", packets, got)
			}
			if got := binary.BigEndian.Uint16(packet[2:4]); got != uint16(65534+packets) {
				t.Fatalf("sequence gap at %d", packets)
			}
			if alac {
				payload, err := DecryptFrames(s.aesKey, s.aesIV, packet[12:])
				if err != nil {
					t.Fatal(err)
				}
				encoded = append(encoded, payload)
			} else {
				received = append(received, packet[12:]...)
			}
			packets++
		}
		// Distributor buffers can be reused as soon as WriteFrame returns.
		clear(pcm)
	}
	if packets != 441 || (!alac && !bytes.Equal(source, received)) || len(s.pendingPCM) != 0 {
		t.Fatalf("PCM changed: packets=%d received=%d source=%d pending=%d", packets, len(received), len(source), len(s.pendingPCM))
	}
	if alac {
		assertDecodedALAC(t, encoded, source)
	}
}

func TestEncryptedALACRetransmissionAcrossSequenceWrap(t *testing.T) {
	s, receiver := datagramSink(t, true)
	s.enc = true
	s.aesKey = bytes.Repeat([]byte{0x42}, 16)
	s.aesIV = bytes.Repeat([]byte{0x73}, 16)
	s.startControlReader(s.control, net.IPv4(127, 0, 0, 1))
	original := make([][]byte, 3)
	for i := range original {
		if err := s.WriteFrame(sink.Frame{SampleIndex: int64(i * 352), Data: bytes.Repeat([]byte{byte(i + 1)}, 352*4)}); err != nil {
			t.Fatal(err)
		}
		original[i] = readPacket(t, receiver)
		payload, err := DecryptFrames(s.aesKey, s.aesIV, original[i][12:])
		if err != nil {
			t.Fatal(err)
		}
		var samples uint32
		for bit := 23; bit < 55; bit++ {
			samples = samples<<1 | uint32((payload[bit/8]>>(7-bit%8))&1)
		}
		if samples != 352 {
			t.Fatalf("ALAC frame has %d samples", samples)
		}
	}
	request := []byte{0x80, 0xd5, 0, 1, 0xff, 0xfe, 0, 3}
	if _, err := receiver.WriteToUDP(request, s.control.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatal(err)
	}
	for i := range original {
		got := readPacket(t, receiver)
		if got[0] != 0x80 || got[1] != 0xd6 || !bytes.Equal(got[2:4], original[i][2:4]) || !bytes.Equal(got[4:], original[i]) {
			t.Fatalf("retransmitted packet %d differs", i)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sent != 1056 || s.resent != 3 || s.pkt.Seq != 1 {
		t.Fatalf("retransmission advanced stream: samples=%d resent=%d seq=%d", s.sent, s.resent, s.pkt.Seq)
	}
}

func TestKeepaliveUsesRTSPAndStopsWithSession(t *testing.T) {
	f := newFakeRTSP(t)
	cl, err := Dial(context.Background(), f.addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	s := New("keepalive", Options{})
	done := make(chan struct{})
	s.startKeepAlive(cl, done, 5*time.Millisecond)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		methods, _ := f.snapshot()
		if len(methods) > 0 {
			close(done)
			if methods[0] != "OPTIONS" {
				t.Fatal(methods)
			}
			// Allow one request already in flight, then verify no new ticks run.
			time.Sleep(20 * time.Millisecond)
			settled, _ := f.snapshot()
			time.Sleep(20 * time.Millisecond)
			after, _ := f.snapshot()
			if len(after) != len(settled) {
				t.Fatal("keepalive continued after session cancellation")
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	close(done)
	t.Fatal("no keepalive request")
}
