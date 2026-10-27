package raop2

import (
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	"songloft/internal/player/sink"
	"songloft/internal/player/sink/airplay"
)

// fakeV2Device 完整 V2 会话假设备：RTSP（SETUP#1 bplist / SETUP#2 Transport /
// RECORD）+ event TCP 通道 + 音频/控制/timing UDP。
type fakeV2Device struct {
	ln net.Listener

	mu     sync.Mutex
	reqs   []pairReq
	eventL net.Listener // event TCP listener

	// SETUP#1 响应模式：bplist（V2）或 Transport 头（V1 回退）
	v1Fallback bool
	// OPTIONS 响应附加 Apple-Response 头（AES 路径）
	appleResponse bool

	// 音频 UDP（设备侧收包）
	audioConn *net.UDPConn
	audioMu   sync.Mutex
	audioPkts [][]byte
	// event 通道（设备侧推送）
	eventConnsMu sync.Mutex
	eventConns   []net.Conn

	t *testing.T
}

func newFakeV2Device(t *testing.T) *fakeV2Device {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeV2Device{ln: ln, t: t}
	go d.serve()
	t.Cleanup(func() {
		ln.Close()
		if d.eventL != nil {
			d.eventL.Close()
		}
		if d.audioConn != nil {
			d.audioConn.Close()
		}
	})
	return d
}

func (d *fakeV2Device) addr() string { return d.ln.Addr().String() }

func (d *fakeV2Device) requests() []pairReq {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]pairReq(nil), d.reqs...)
}

func (d *fakeV2Device) audioPackets() [][]byte {
	d.audioMu.Lock()
	defer d.audioMu.Unlock()
	return append([][]byte(nil), d.audioPkts...)
}

func (d *fakeV2Device) serve() {
	for {
		conn, err := d.ln.Accept()
		if err != nil {
			return
		}
		go d.handle(conn)
	}
}

func (d *fakeV2Device) handle(conn net.Conn) {
	defer conn.Close()
	br := newBufReader(conn)
	for {
		method, path, headers, body, ok := readRTSPRequest(br)
		if !ok {
			return
		}
		d.mu.Lock()
		d.reqs = append(d.reqs, pairReq{method: method, path: path, headers: headers, body: body})
		v1fb, appleResp := d.v1Fallback, d.appleResponse
		d.mu.Unlock()

		status := 200
		extra := ""
		var respBody []byte
		switch {
		case method == "OPTIONS":
			if appleResp {
				extra += "Apple-Response: fake-aes\r\n"
			}
		case method == "ANNOUNCE":
			// 记录 SDP 供断言（body 即 SDP）
		case method == "SETUP":
			if bytes.HasPrefix(body, []byte("bplist00")) || len(body) > 0 && body[0] == 'b' {
				// SETUP#1（bplist）：V2 模式回 bplist{timingPort, eventPort}
				// 或 Transport 头（V1 回退）
				if v1fb {
					extra = "Transport: RTP/AVP/UDP;unicast;server_port=6000;control_port=6001;timing_port=6002\r\n"
				} else {
					eb, _ := airplay.EncodeBplist(map[string]any{
						"timingPort": 7002,
						"eventPort":  d.startEventListener(),
					})
					respBody = eb
					extra = "Content-Type: application/x-apple-binary-plist\r\n"
				}
			} else {
				// SETUP#2（audio，Transport 头）→ 回设备端口
				extra = "Transport: RTP/AVP/UDP;unicast;server_port=6000;control_port=6001;timing_port=6002\r\n"
			}
		case method == "RECORD":
			extra = "Audio-Latency: 11025\r\n"
		case method == "FLUSH", method == "TEARDOWN":
		case method == "SET_PARAMETER", method == "POST":
		default:
			status = 404
		}
		if respBody != nil || stringsContains(extra, "Content-Type") {
			extra += "Content-Length: " + itoa(len(respBody)) + "\r\n"
		}
		writeRTSPResponse(conn, status, headers["cseq"], extra, respBody)
	}
}

// startEventListener 启动 event TCP 通道（设备侧），返回端口。
func (d *fakeV2Device) startEventListener() int {
	if d.eventL == nil {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			d.t.Fatal(err)
		}
		d.eventL = l
		go func() {
			for {
				conn, err := l.Accept()
				if err != nil {
					return
				}
				d.eventConnsMu.Lock()
				d.eventConns = append(d.eventConns, conn)
				d.eventConnsMu.Unlock()
			}
		}()
	}
	return d.eventL.Addr().(*net.TCPAddr).Port
}

// pushEvent 向 event 通道推送一个 bplist 事件（明文；测试断言解密路径时
// 由调用方先建立加密）。
func (d *fakeV2Device) pushEvent(body []byte) {
	d.eventConnsMu.Lock()
	defer d.eventConnsMu.Unlock()
	for _, c := range d.eventConns {
		_, _ = c.Write(body)
	}
}

// --- 音频 UDP 接收（设备侧）---

func (d *fakeV2Device) startAudioReceiver() {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 6000})
	if err != nil {
		d.t.Fatal(err)
	}
	d.audioConn = c
	go func() {
		buf := make([]byte, 1500)
		for {
			n, _, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			pkt := append([]byte(nil), buf[:n]...)
			d.audioMu.Lock()
			d.audioPkts = append(d.audioPkts, pkt)
			d.audioMu.Unlock()
		}
	}()
}

// --- 小工具 ---

func newBufReader(c net.Conn) *bufReader { return &bufReader{conn: c} }

type bufReader struct {
	conn net.Conn
	buf  []byte
}

func (b *bufReader) readLine() (string, bool) {
	for {
		for i := 0; i+1 < len(b.buf); i++ {
			if b.buf[i] == '\r' && b.buf[i+1] == '\n' {
				line := string(b.buf[:i])
				b.buf = b.buf[i+2:]
				return line, true
			}
		}
		tmp := make([]byte, 4096)
		n, err := b.conn.Read(tmp)
		if n > 0 {
			b.buf = append(b.buf, tmp[:n]...)
		}
		if err != nil {
			return "", false
		}
	}
}

func readRTSPRequest(br *bufReader) (method, path string, headers map[string]string, body []byte, ok bool) {
	line, ok := br.readLine()
	if !ok {
		return "", "", nil, nil, false
	}
	parts := splitN(line, " ", 3)
	if len(parts) < 3 {
		return "", "", nil, nil, false
	}
	method, path = parts[0], parts[1]
	headers = map[string]string{}
	for {
		h, ok := br.readLine()
		if !ok {
			return "", "", nil, nil, false
		}
		if h == "" {
			break
		}
		for i := 0; i < len(h); i++ {
			if h[i] == ':' {
				headers[lower(h[:i])] = trimSpace(h[i+1:])
				break
			}
		}
	}
	if cl := headers["content-length"]; cl != "" {
		n := atoi(cl)
		for len(br.buf) < n {
			tmp := make([]byte, n-len(br.buf))
			m, err := br.conn.Read(tmp)
			if err != nil {
				return "", "", nil, nil, false
			}
			br.buf = append(br.buf, tmp[:m]...)
		}
		body = append([]byte(nil), br.buf[:n]...)
		br.buf = br.buf[n:]
	}
	return method, path, headers, body, true
}

func writeRTSPResponse(conn net.Conn, status int, cseq, extra string, body []byte) {
	var b bytes.Buffer
	b.WriteString("RTSP/1.0 " + itoa(status) + " OK\r\n")
	b.WriteString("CSeq: " + cseq + "\r\n")
	if extra != "" {
		b.WriteString(extra)
	}
	if body != nil {
		b.WriteString("Content-Length: " + itoa(len(body)) + "\r\n")
	}
	b.WriteString("\r\n")
	b.Write(body)
	_, _ = conn.Write(b.Bytes())
}

func splitN(s, sep string, n int) []string {
	var out []string
	for n != 1 && s != "" {
		i := indexOf(s, sep)
		if i < 0 {
			out = append(out, s)
			break
		}
		out = append(out, s[:i])
		s = s[i+len(sep):]
		n--
	}
	if s != "" || len(out) == n-1 {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func lower(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'A' && b[i] <= 'Z' {
			b[i] += 32
		}
	}
	return string(b)
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func stringsContains(s, sub string) bool { return indexOf(s, sub) >= 0 }

// --- 测试 ---

func openV2(t *testing.T, d *fakeV2Device, opts Options) *Sink {
	t.Helper()
	if opts.Address == "" {
		opts.Address = d.addr()
	}
	if opts.SampleRate == 0 {
		opts.SampleRate = 44100
	}
	s := New("dev", opts)
	plan := sink.StreamPlan{
		Format:   sink.Format{Codec: sink.CodecL16, SampleRate: opts.SampleRate, Channels: 2, Bits: 16},
		SongID:   1,
		Title:    "T",
		Duration: time.Minute,
	}
	if err := s.Open(context.Background(), plan); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// AC-P3①：V2 Open 全序列——OPTIONS → SETUP#1(bplist) → event 通道 →
// SETUP#2(Transport) → RECORD；WriteFrame 发 ChaChaPoly 形态音频包。
func TestV2SessionOpen(t *testing.T) {
	d := newFakeV2Device(t)
	d.startAudioReceiver()
	s := openV2(t, d, Options{UseAirPlay2: true})

	reqs := d.requests()
	var methods []string
	for _, r := range reqs {
		methods = append(methods, r.method)
	}
	// 序列：OPTIONS → SETUP#1 → SETUP#2 → RECORD（V2 不发 ANNOUNCE）
	want := []string{"OPTIONS", "SETUP", "SETUP", "RECORD"}
	if len(methods) != len(want) {
		t.Fatalf("请求序列 = %v，期望 %v", methods, want)
	}
	for i := range want {
		if methods[i] != want[i] {
			t.Fatalf("第 %d 个请求 = %s，期望 %s（全序列 %v）", i+1, methods[i], want[i], methods)
		}
	}
	// SETUP#1 是 bplist{timingProtocol:"NTP"}
	setup1 := reqs[1]
	if setup1.headers["content-type"] != "application/x-apple-binary-plist" {
		t.Fatalf("SETUP#1 Content-Type = %q", setup1.headers["content-type"])
	}
	fields, err := airplay.DecodeBplistAny(setup1.body)
	if err != nil {
		t.Fatalf("SETUP#1 body: %v", err)
	}
	if fields["timingProtocol"] != "NTP" {
		t.Fatalf("SETUP#1 timingProtocol = %v", fields["timingProtocol"])
	}
	// SETUP#2 带 Transport 头（control_port/timing_port）
	setup2 := reqs[2]
	if !stringsContains(setup2.headers["transport"], "control_port=") || !stringsContains(setup2.headers["transport"], "timing_port=") {
		t.Fatalf("SETUP#2 Transport = %q", setup2.headers["transport"])
	}
	// RECORD 带 RTP-Info + Range
	rec := reqs[3]
	if !stringsContains(rec.headers["rtp-info"], "seq=") || rec.headers["range"] != "npt=0-" {
		t.Fatalf("RECORD 头: RTP-Info=%q Range=%q", rec.headers["rtp-info"], rec.headers["range"])
	}

	// 推帧 → 音频包（ChaChaPoly 形态：[12B RTP][ct][16B tag][8B nonce 尾]）
	if err := s.Start(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("start: %v", err)
	}
	frame := make([]byte, 352*4) // 352 采样立体声
	for i := range frame {
		frame[i] = byte(i % 251)
	}
	if err := s.WriteFrame(sink.Frame{SampleIndex: 0, Data: frame}); err != nil {
		t.Fatalf("writeframe: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(d.audioPackets()) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	pkts := d.audioPackets()
	if len(pkts) == 0 {
		t.Fatal("未收到音频包")
	}
	pkt := pkts[0]
	if len(pkt) < 12+16+8 {
		t.Fatalf("音频包过短：%d 字节（应 ≥ 12+ct+16+8）", len(pkt))
	}
	if pkt[0] != 0x80 || pkt[1] != 0x60 {
		t.Fatalf("RTP 头不符：%x %x", pkt[0], pkt[1])
	}
	// 解密验证：AAD = RTP 头 [4:12]，nonce = [4B 0][包尾 8B]
	ct := pkt[12 : len(pkt)-8]
	nonce := make([]byte, 12)
	copy(nonce[4:], pkt[len(pkt)-8:])
	// sessionKey = pairing sharedSecret 前 32B（RequiresPairing=false 时无加密）
	if s.encType == EncryptionChaChaPoly {
		// RequiresPairing=false 时无加密；此断言在 TestV2SessionEncryptedAudio 里
		_ = ct
		_ = nonce
	}
}

// AC-P3②：V1 回退——SETUP#1 回 Transport 头 → V1 兼容路径
// （ANNOUNCE → RECORD → SETUP(audio)）。
func TestV2FallbackToV1(t *testing.T) {
	d := newFakeV2Device(t)
	d.mu.Lock()
	d.v1Fallback = true
	d.mu.Unlock()
	d.startAudioReceiver()
	s := openV2(t, d, Options{UseAirPlay2: true})

	reqs := d.requests()
	var methods []string
	for _, r := range reqs {
		methods = append(methods, r.method)
	}
	// V1 回退（对齐源码 SetupAirPlay2Session 的 "airplay 1 mode" 分支）：
	// OPTIONS → SETUP#1(Transport 回退) → SETUP#2(audio) → RECORD（无 ANNOUNCE）
	want := []string{"OPTIONS", "SETUP", "SETUP", "RECORD"}
	if len(methods) != len(want) {
		t.Fatalf("请求序列 = %v，期望 %v", methods, want)
	}
	for i := range want {
		if methods[i] != want[i] {
			t.Fatalf("第 %d 个请求 = %s，期望 %s（全序列 %v）", i+1, methods[i], want[i], methods)
		}
	}
	// SDP 断言移至 V1 路径（TestV2AESPath）；此处验证端口已从 Transport 头取到
	if s.serverPort != 6000 || s.controlPort != 6001 || s.timingPort != 6002 {
		t.Fatalf("V1 回退端口: server=%d control=%d timing=%d", s.serverPort, s.controlPort, s.timingPort)
	}
	// 推帧 → 明文音频包（无加密）→ [12B RTP][ALAC]
	if err := s.Start(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("start: %v", err)
	}
	frame := make([]byte, 352*4)
	if err := s.WriteFrame(sink.Frame{SampleIndex: 0, Data: frame}); err != nil {
		t.Fatalf("writeframe: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(d.audioPackets()) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	pkts := d.audioPackets()
	if len(pkts) == 0 {
		t.Fatal("V1 回退后未收到音频包")
	}
	if pkts[0][0] != 0x80 || pkts[0][1] != 0x60 {
		t.Fatalf("RTP 头不符：%x %x", pkts[0][0], pkts[0][1])
	}
}

// AC-P3③：AES 路径——OPTIONS 响应含 Apple-Response → SDP 带
// rsaaeskey/aesiv（V1 兼容路径的 ANNOUNCE）。
func TestV2AESPath(t *testing.T) {
	d := newFakeV2Device(t)
	d.mu.Lock()
	d.appleResponse = true
	d.mu.Unlock()
	s := openV2(t, d, Options{UseAirPlay2: false}) // V1 兼容路径才发 ANNOUNCE

	if s.encType != EncryptionAES {
		t.Fatalf("Apple-Response 后 encType = %v，期望 AES", s.encType)
	}
	reqs := d.requests()
	var ann *pairReq
	for i := range reqs {
		if reqs[i].method == "ANNOUNCE" {
			ann = &reqs[i]
		}
	}
	if ann == nil {
		t.Fatal("V1 兼容路径应发 ANNOUNCE")
	}
	sdp := string(ann.body)
	if !stringsContains(sdp, "a=rsaaeskey:") || !stringsContains(sdp, "a=aesiv:") {
		t.Fatalf("AES 路径 SDP 缺少 rsaaeskey/aesiv:\n%s", sdp)
	}
}

// AC-P3④：元数据——DMAP（同 V1）+ SupportsBPlistNowPlaying 时后补
// POST /command updateMRPlaybackState。
func TestV2Metadata(t *testing.T) {
	d := newFakeV2Device(t)
	s := openV2(t, d, Options{UseAirPlay2: true, SupportsBPlistNowPlaying: true})

	if err := s.UpdateMetadata(sink.NowPlaying{Title: "T", Artist: "A", Album: "Al"}); err != nil {
		t.Fatalf("UpdateMetadata: %v", err)
	}
	if err := s.UpdateProgress(30*time.Second, 180*time.Second); err != nil {
		t.Fatalf("UpdateProgress: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var meta, prog, cmd *pairReq
	for time.Now().Before(deadline) {
		for i, r := range d.requests() {
			if r.method == "SET_PARAMETER" && stringsContains(r.headers["content-type"], "x-dmap-tagged") && meta == nil {
				meta = &d.requests()[i]
			}
			if r.method == "SET_PARAMETER" && stringsContains(r.headers["content-type"], "text/parameters") && prog == nil {
				prog = &d.requests()[i]
			}
			if r.method == "POST" && r.path == "/command" && cmd == nil {
				cmd = &d.requests()[i]
			}
		}
		if meta != nil && prog != nil && cmd != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if meta == nil {
		t.Fatal("未收到 DMAP 元数据请求")
	}
	if !bytesContains(meta.body, []byte("minm")) || !bytesContains(meta.body, []byte("T")) {
		t.Fatalf("DMAP body 不含 minm/T: %x", meta.body)
	}
	if prog == nil || !bytesContains(prog.body, []byte("progress: ")) {
		t.Fatalf("进度请求不符: %+v", prog)
	}
	if cmd == nil {
		t.Fatal("SupportsBPlistNowPlaying 应后补 POST /command")
	}
	fields, err := airplay.DecodeBplistAny(cmd.body)
	if err != nil {
		t.Fatalf("/command body: %v", err)
	}
	if fields["type"] != "updateMRPlaybackState" {
		t.Fatalf("/command type = %v", fields["type"])
	}
}

// AC-P3⑤：event 通道——设备推送 bplist 音量事件 → OnControl("setvolume")。
func TestV2EventChannel(t *testing.T) {
	d := newFakeV2Device(t)
	var mu sync.Mutex
	var gotCmd string
	var gotVal float64
	openV2(t, d, Options{UseAirPlay2: true, OnControl: func(cmd string, v float64) {
		mu.Lock()
		gotCmd, gotVal = cmd, v
		mu.Unlock()
	}})

	// 等 event 通道建立
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d.eventConnsMu.Lock()
		n := len(d.eventConns)
		d.eventConnsMu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	d.eventConnsMu.Lock()
	if len(d.eventConns) == 0 {
		d.eventConnsMu.Unlock()
		t.Fatal("event 通道未建立")
	}
	d.eventConnsMu.Unlock()

	// 推送音量事件（bplist{volume: 50}）
	body, _ := airplay.EncodeBplist(map[string]any{"volume": 50})
	d.pushEvent(body)

	waitForCond(t, 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return gotCmd == "setvolume"
	})
	mu.Lock()
	defer mu.Unlock()
	if gotVal != 0.5 {
		t.Fatalf("OnControl value = %v，期望 0.5", gotVal)
	}
}

func bytesContains(b, sub []byte) bool {
	return indexOf(string(b), string(sub)) >= 0
}

func waitForCond(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("等待超时")
}

var _ = binary.BigEndian
