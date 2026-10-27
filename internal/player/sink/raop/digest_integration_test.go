package raop

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"songloft/internal/player/sink"
)

// authRTSP 假设备：首个 OPTIONS 回 401+WWW-Authenticate，带 Authorization
// 头的请求回 200（对齐密码音箱的挑战行为）。
type authRTSP struct {
	ln       net.Listener
	mu       sync.Mutex
	authSeen []string // 每个请求的 Authorization 头（无则空串）
	dataPort int
}

func newAuthRTSP(t *testing.T) *authRTSP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	a := &authRTSP{ln: ln, dataPort: udp.LocalAddr().(*net.UDPAddr).Port}
	go a.serve()
	t.Cleanup(func() { ln.Close() })
	return a
}

func (a *authRTSP) addr() string { return a.ln.Addr().String() }

func (a *authRTSP) serve() {
	for {
		conn, err := a.ln.Accept()
		if err != nil {
			return
		}
		go a.handle(conn)
	}
}

func (a *authRTSP) handle(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	challengeOnce := false
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
		if n := headers["content-length"]; n != "" {
			var size int
			fmt.Sscanf(n, "%d", &size)
			buf := make([]byte, size)
			if _, err := readFull(br, buf); err != nil {
				return
			}
		}
		a.mu.Lock()
		a.authSeen = append(a.authSeen, headers["authorization"])
		a.mu.Unlock()

		extra := ""
		status := 200
		if method == "OPTIONS" && !challengeOnce && headers["authorization"] == "" {
			// 首个无凭据 OPTIONS：发挑战
			challengeOnce = true
			status = 401
			extra = `WWW-Authenticate: Digest realm="Music", nonce="abc"` + "\r\n"
		}
		if method == "SETUP" {
			extra += fmt.Sprintf("Transport: RTP/AVP/UDP;unicast;server_port=%d;control_port=%d;timing_port=%d\r\n",
				a.dataPort, a.dataPort+1, a.dataPort+2)
		}
		if method != "OPTIONS" {
			extra += "Session: 11223344\r\n"
		}
		fmt.Fprintf(conn, "RTSP/1.0 %d Test\r\nCSeq: %s\r\n%s\r\n", status, headers["cseq"], extra)
	}
}

func (a *authRTSP) authorizations() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.authSeen...)
}

// TestSinkPasswordDigest 带密码的 Open：401 → 算 digest → 重试成功，
// 且授权头逐字等于期望值、随后续请求复用；无密码遇 401 → 明确报错。
func TestSinkPasswordDigest(t *testing.T) {
	// 1) 有密码：完整握手成功。
	f := newAuthRTSP(t)
	s := New("dev", Options{
		Address: f.addr(), SampleRate: 44100, Channels: 2,
		Latency: 2 * time.Second, Password: "pw",
	})
	if err := s.Open(context.Background(), sink.StreamPlan{
		Format: sink.Format{Codec: sink.CodecL16, SampleRate: 44100, Channels: 2, Bits: 16},
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	auths := f.authorizations()
	if len(auths) < 2 {
		t.Fatalf("期望 ≥2 个请求，实际 %d", len(auths))
	}
	if auths[0] != "" {
		t.Fatalf("首个 OPTIONS 不应带授权头: %q", auths[0])
	}
	want := authorizationHeader("Music", "abc", "*", computeDigest("Music", "pw", "OPTIONS", "*", "abc"))
	if auths[1] != want {
		t.Fatalf("重试的授权头不符:\n got=%q\nwant=%q", auths[1], want)
	}
	for i, a := range auths[2:] {
		if a != want {
			t.Fatalf("第 %d 个后续请求的授权头未复用: %q", i+2, a)
		}
	}

	// 2) 无密码遇 401：Open 失败且错误信息含 401。
	f2 := newAuthRTSP(t)
	s2 := New("dev", Options{Address: f2.addr(), SampleRate: 44100, Channels: 2})
	err := s2.Open(context.Background(), sink.StreamPlan{
		Format: sink.Format{Codec: sink.CodecL16, SampleRate: 44100, Channels: 2, Bits: 16},
	})
	if err == nil {
		s2.Close()
		t.Fatal("无密码遇 401 应当失败")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("错误信息应含 401: %v", err)
	}
}
