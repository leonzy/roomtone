package raop

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"songloft/internal/player/sink"
)

func TestBuildRequestGolden(t *testing.T) {
	req := BuildRequest("ANNOUNCE", "rtsp://192.0.2.10:7000/AA:BB:CC:DD:EE:FF", 3, "SESS",
		map[string]string{"Content-Type": "application/sdp"}, []byte("v=0\r\n"))
	want := "ANNOUNCE rtsp://192.0.2.10:7000/AA:BB:CC:DD:EE:FF RTSP/1.0\r\n" +
		"CSeq: 3\r\n" +
		"Session: SESS\r\n" +
		"Content-Type: application/sdp\r\n" +
		"Content-Length: 5\r\n" +
		"\r\n" +
		"v=0\r\n"
	if string(req) != want {
		t.Fatalf("报文不符:\n got=%q\nwant=%q", req, want)
	}
}

func TestReadResponse(t *testing.T) {
	raw := "RTSP/1.0 200 OK\r\nCSeq: 2\r\nSession: 1234;timeout=60\r\nContent-Length: 3\r\n\r\nabc"
	resp, err := ReadResponse(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != 200 || string(resp.Body) != "abc" {
		t.Fatalf("解析错误: %+v", resp)
	}
	if got := resp.HeaderValue("session"); got != "1234;timeout=60" {
		t.Fatalf("头字段大小写不敏感取值失败: %q", got)
	}
}

func TestBuildSDP(t *testing.T) {
	got := string(BuildSDP(SDP{Origin: "192.0.2.10", SessionID: 42}))
	want := "v=0\r\n" +
		"o=iTunes 42 0 IN IP4 192.0.2.10\r\n" +
		"s=iTunes\r\n" +
		"c=IN IP4 192.0.2.10\r\n" +
		"t=0 0\r\n" +
		"m=audio 0 RTP/AVP 96\r\n" +
		"a=rtpmap:96 L16/44100/2\r\n" +
		"a=ptime:352\r\n" +
		"a=maxptime:352\r\n" +
		"a=minptime:352\r\n"
	if got != want {
		t.Fatalf("SDP 不符:\n got=%q\nwant=%q", got, want)
	}
	if strings.Contains(got, "rsaaeskey") {
		t.Fatal("明文模式不应声明 rsaaeskey")
	}
}

// NIST SP 800-38A F.2.1 CBC-AES128 第一块，用来钉住 CBC 实现。
func TestParseTransportPorts(t *testing.T) {
	s, c := ParseTransportPorts("RTP/AVP/UDP;unicast;server_port=7100;control_port=7101;timing_port=7102")
	if s != 7100 || c != 7101 {
		t.Fatalf("端口解析错误: %d %d", s, c)
	}
	s, c = ParseTransportPorts("")
	if s != 6000 || c != 6001 {
		t.Fatalf("缺省端口错误: %d %d", s, c)
	}
}

func TestParseTXT(t *testing.T) {
	caps := ParseTXT([]string{"ch=2", "cn=0,1", "sr=44100", "ss=16", "pw=true", "tp=UDP"})
	if caps.Channels != 2 || caps.MaxSampleRate != 44100 || caps.BitDepth != 16 || !caps.NeedsPassword {
		t.Fatalf("TXT 解析错误: %+v", caps)
	}
}

// fakeRTSP 假设备：记录方法序列，SETUP 返回设备端口。
type fakeRTSP struct {
	ln        net.Listener
	mu        sync.Mutex
	methods   []string
	bodies    []string
	dataPort  int
	onRequest func(method string, headers map[string]string) int
}

func newFakeRTSP(t *testing.T) *fakeRTSP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRTSP{ln: ln}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeRTSP) addr() string { return f.ln.Addr().String() }

func (f *fakeRTSP) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeRTSP) handle(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		parts := strings.SplitN(strings.TrimSpace(line), " ", 3)
		if len(parts) < 3 {
			return
		}
		method := parts[0]
		headers := map[string]string{}
		for {
			h, err := br.ReadString('\n')
			if err != nil {
				return
			}
			h = strings.TrimRight(h, "\r\n")
			if h == "" {
				break
			}
			k, v, ok := strings.Cut(h, ":")
			if ok {
				headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
			}
		}
		body := ""
		if n := headers["content-length"]; n != "" {
			var size int
			fmt.Sscanf(n, "%d", &size)
			buf := make([]byte, size)
			if _, err := readFull(br, buf); err != nil {
				return
			}
			body = string(buf)
		}
		f.mu.Lock()
		f.methods = append(f.methods, method)
		f.bodies = append(f.bodies, body)
		f.mu.Unlock()

		extra := ""
		switch method {
		case "SETUP":
			extra = fmt.Sprintf("Transport: RTP/AVP/UDP;unicast;server_port=%d;control_port=%d;timing_port=%d\r\n",
				f.dataPort, f.dataPort+1, f.dataPort+2)
		}
		if method != "OPTIONS" {
			extra += "Session: 11223344\r\n"
		}
		status := 200
		if f.onRequest != nil {
			if got := f.onRequest(method, headers); got != 0 {
				status = got
			}
		}
		fmt.Fprintf(conn, "RTSP/1.0 %d Test\r\nCSeq: %s\r\n%s\r\n", status, headers["cseq"], extra)
	}
}

func readFull(br *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := br.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (f *fakeRTSP) snapshot() ([]string, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.methods...), append([]string(nil), f.bodies...)
}

func TestSinkHandshakeAndStream(t *testing.T) {
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()

	f := newFakeRTSP(t)
	f.dataPort = udp.LocalAddr().(*net.UDPAddr).Port

	s := New("p1", Options{Address: f.addr(), DeviceID: "AA:BB:CC:DD:EE:FF"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Open(ctx, sink.StreamPlan{Format: sink.Format{Codec: sink.CodecL16, SampleRate: 44100, Channels: 2, Bits: 16}}); err != nil {
		t.Fatalf("Open 失败: %v", err)
	}

	if err := s.Start(time.Now()); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}

	// 20ms 立体声 16bit = 3528 字节。
	frame := sink.Frame{SampleIndex: 0, Data: make([]byte, 3528)}
	if err := s.WriteFrame(frame); err != nil {
		t.Fatalf("WriteFrame 失败: %v", err)
	}

	buf := make([]byte, 4096)
	udp.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := udp.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("没收到 RTP 包: %v", err)
	}
	if n != 12+352*4 {
		t.Fatalf("RTP 包长度 %d，期望 %d", n, 12+352*4)
	}
	firstTimestamp := binary.BigEndian.Uint32(buf[4:8])
	// Keep the last 178 samples buffered for the next distributor frame.
	for i, samples := range []int{352} {
		n, _, err = udp.ReadFromUDP(buf)
		if err != nil || n != 12+samples*4 {
			t.Fatalf("split packet %d: bytes=%d err=%v", i+1, n, err)
		}
		if got := binary.BigEndian.Uint32(buf[4:8]); got != firstTimestamp+uint32((i+1)*352) {
			t.Fatalf("split timestamp: %d", got)
		}
	}
	if buf[1]&0x7f != PayloadType {
		t.Fatalf("payload type 错误: %02x", buf[1])
	}

	if err := s.SetVolume(0.5); err != nil {
		t.Fatalf("SetVolume 失败: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	methods, bodies := f.snapshot()
	want := []string{"OPTIONS", "ANNOUNCE", "SETUP", "RECORD", "SET_PARAMETER", "SET_PARAMETER", "TEARDOWN"}
	if strings.Join(methods, ",") != strings.Join(want, ",") {
		t.Fatalf("调用序列错误:\n got=%v\nwant=%v", methods, want)
	}
	if !strings.Contains(bodies[1], "m=audio 0 RTP/AVP 96") {
		t.Fatalf("ANNOUNCE body 不含 SDP: %q", bodies[1])
	}
	if !strings.Contains(bodies[4], "volume: 0.00") || !strings.Contains(bodies[5], "volume: -6.02") {
		t.Fatalf("音量参数错误: initial=%q updated=%q", bodies[4], bodies[5])
	}
}

func TestSinkOpenRejectsBadStatus(t *testing.T) {
	f := newFakeRTSP(t)
	f.onRequest = func(string, map[string]string) int { return 453 }
	s := New("p2", Options{Address: f.addr()})
	defer s.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.Open(ctx, sink.StreamPlan{})
	if err == nil || !strings.Contains(err.Error(), "453") {
		t.Fatalf("应返回 453 错误，实际 %v", err)
	}
	methods, _ := f.snapshot()
	if len(methods) != 3 {
		t.Fatalf("retry count=%d", len(methods))
	}
}
