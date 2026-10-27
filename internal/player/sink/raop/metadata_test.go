package raop

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"songloft/internal/player/sink"
)

// recordingRTSP 假设备：完整应答握手，并记录每个请求的方法/头/体。
type recordingRTSP struct {
	ln       net.Listener
	mu       sync.Mutex
	reqs     []recordedRequest
	dataPort int
}

type recordedRequest struct {
	method  string
	headers map[string]string
	body    []byte
}

func newRecordingRTSP(t *testing.T) *recordingRTSP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// 给 SETUP 响应一个假的数据端口（本测试不收流）。
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	f := &recordingRTSP{ln: ln, dataPort: udp.LocalAddr().(*net.UDPAddr).Port}
	go f.serve()
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *recordingRTSP) addr() string { return f.ln.Addr().String() }

func (f *recordingRTSP) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *recordingRTSP) handle(conn net.Conn) {
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
		body := []byte{}
		if n := headers["content-length"]; n != "" {
			var size int
			fmt.Sscanf(n, "%d", &size)
			buf := make([]byte, size)
			if _, err := readFull(br, buf); err != nil {
				return
			}
			body = buf
		}
		f.mu.Lock()
		f.reqs = append(f.reqs, recordedRequest{method: method, headers: headers, body: body})
		f.mu.Unlock()

		extra := ""
		if method == "SETUP" {
			extra = fmt.Sprintf("Transport: RTP/AVP/UDP;unicast;server_port=%d;control_port=%d;timing_port=%d\r\n",
				f.dataPort, f.dataPort+1, f.dataPort+2)
		}
		if method != "OPTIONS" {
			extra += "Session: 11223344\r\n"
		}
		fmt.Fprintf(conn, "RTSP/1.0 200 Test\r\nCSeq: %s\r\n%s\r\n", headers["cseq"], extra)
	}
}

// setParameters 取所有 Content-Type 过滤后的 SET_PARAMETER 请求。
func (f *recordingRTSP) setParameters(ctype string) []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []recordedRequest
	for _, r := range f.reqs {
		if r.method == "SET_PARAMETER" && strings.Contains(r.headers["content-type"], ctype) {
			out = append(out, r)
		}
	}
	return out
}

func (f *recordingRTSP) all() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.reqs...)
}

func waitForReqs(t *testing.T, what string, f *recordingRTSP, ctype string, n int) []recordedRequest {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if reqs := f.setParameters(ctype); len(reqs) >= n {
			return reqs
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s（Content-Type %s 只收到 %d 个）", what, ctype, len(f.setParameters(ctype)))
	return nil
}

// openStarted 完成握手并把播放锚定到 30s 前（可闻位置 ≈ 30s）。
func openStarted(t *testing.T, f *recordingRTSP) *Sink {
	t.Helper()
	s := New("dev", Options{Address: f.addr(), SampleRate: 44100, Channels: 2, Latency: 2 * time.Second})
	plan := sink.StreamPlan{
		Format:   sink.Format{Codec: sink.CodecL16, SampleRate: 44100, Channels: 2, Bits: 16},
		SongID:   1,
		Title:    "T",
		Duration: time.Minute,
	}
	if err := s.Open(context.Background(), plan); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Start(time.Now().Add(-30 * time.Second)); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestUpdateMetadataDMAP 元数据走 SET_PARAMETER + DMAP，逐字对齐
// AirPlayClientV1.UpdateMetadata（mlit{mikd=2,asal,asar,asaa,minm}）。
func TestUpdateMetadataDMAP(t *testing.T) {
	f := newRecordingRTSP(t)
	s := openStarted(t, f)

	if err := s.UpdateMetadata(sink.NowPlaying{Title: "Title", Artist: "Artist", Album: "Album"}); err != nil {
		t.Fatalf("UpdateMetadata: %v", err)
	}
	reqs := waitForReqs(t, "元数据推送", f, "application/x-dmap-tagged", 1)
	r := reqs[0]
	if r.headers["rtp-info"] == "" || !strings.HasPrefix(r.headers["rtp-info"], "rtptime=") {
		t.Fatalf("缺少 RTP-Info 头: %q", r.headers["rtp-info"])
	}
	// golden 与 TestDMAPEncoderGolden 同源：mlit{mikd=2,asal=Album,asar=Artist,asaa=Artist,minm=Title}
	want := "6d6c69740000003f" +
		"6d696b640000000102" +
		"6173616c00000005" + "416c62756d" +
		"6173617200000006" + "417274697374" +
		"6173616100000006" + "417274697374" +
		"6d696e6d00000005" + "5469746c65"
	if got := fmt.Sprintf("%x", r.body); got != want {
		t.Fatalf("DMAP body 不符:\n got=%s\nwant=%s", got, want)
	}
}

// TestUpdateProgress 进度体为 progress: start/cur/end\r\n（RTP 时间戳），
// cur-start=位置×采样率、end-cur=剩余×采样率（cur 取发送时刻，容差 4410）。
func TestUpdateProgress(t *testing.T) {
	f := newRecordingRTSP(t)
	s := openStarted(t, f)

	if err := s.UpdateProgress(30*time.Second, 180*time.Second); err != nil {
		t.Fatalf("UpdateProgress: %v", err)
	}
	reqs := waitForReqs(t, "进度推送", f, "text/parameters", 1)
	r := reqs[len(reqs)-1]
	body := string(r.body)
	if !strings.HasPrefix(body, "progress: ") || !strings.HasSuffix(body, "\r\n") {
		t.Fatalf("进度体格式不符: %q", body)
	}
	fields := strings.Split(strings.TrimSpace(strings.TrimPrefix(body, "progress: ")), "/")
	if len(fields) != 3 {
		t.Fatalf("进度体应为三段: %q", body)
	}
	vals := make([]int64, 3)
	for i, fs := range fields {
		v, err := strconv.ParseInt(fs, 10, 64)
		if err != nil {
			t.Fatalf("进度值 %q 非数字: %v", fs, err)
		}
		vals[i] = v
	}
	start, cur, end := vals[0], vals[1], vals[2]
	// 位置 30s：cur-start = 30×44100（cur 取发送时刻，容差 4410 ≈ 100ms）
	if d := cur - start - int64(30*44100); d < -4410 || d > 4410 {
		t.Fatalf("cur-start = %d，期望 %d±4410", cur-start, 30*44100)
	}
	// 剩余 150s：end-cur = 150×44100（同一 cur 推出，精确）
	if d := end - cur - int64(150*44100); d != 0 {
		t.Fatalf("end-cur = %d，期望 %d", end-cur, 150*44100)
	}
}

// TestUpdateArtwork 封面走 SET_PARAMETER image/jpeg；非 JPEG 输入转码；
// 空路径发 image/none 清屏（对齐 AirPlayClientV1.UpdateImage）。
func TestUpdateArtwork(t *testing.T) {
	f := newRecordingRTSP(t)
	s := openStarted(t, f)

	// 1) JPEG 原样透传。
	dir := t.TempDir()
	jpgPath := filepath.Join(dir, "cover.jpg")
	var jpgBytes []byte
	{
		img := image.NewRGBA(image.Rect(0, 0, 4, 4))
		img.Set(0, 0, color.RGBA{R: 255, A: 255})
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, nil); err != nil {
			t.Fatal(err)
		}
		jpgBytes = buf.Bytes()
		if err := os.WriteFile(jpgPath, jpgBytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpdateArtwork(jpgPath); err != nil {
		t.Fatalf("UpdateArtwork: %v", err)
	}
	reqs := waitForReqs(t, "JPEG 封面", f, "image/jpeg", 1)
	if !bytes.Equal(reqs[0].body, jpgBytes) {
		t.Fatalf("JPEG body 应原样透传（%d vs %d 字节）", len(reqs[0].body), len(jpgBytes))
	}

	// 2) PNG 输入被转码为 JPEG。
	pngPath := filepath.Join(dir, "cover.png")
	{
		img := image.NewRGBA(image.Rect(0, 0, 4, 4))
		img.Set(1, 1, color.RGBA{B: 255, A: 255})
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pngPath, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpdateArtwork(pngPath); err != nil {
		t.Fatalf("UpdateArtwork(png): %v", err)
	}
	reqs = waitForReqs(t, "PNG 转码", f, "image/jpeg", 2)
	if _, err := jpeg.Decode(bytes.NewReader(reqs[1].body)); err != nil {
		t.Fatalf("PNG 输入应被转码为合法 JPEG: %v", err)
	}

	// 3) 空路径 → image/none 清屏。
	if err := s.UpdateArtwork(""); err != nil {
		t.Fatalf("UpdateArtwork(空): %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var none []recordedRequest
	for time.Now().Before(deadline) {
		if none = f.setParameters("image/none"); len(none) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if len(none) == 0 {
		t.Fatal("等待超时：image/none 清屏请求")
	}
	if len(none[0].body) != 0 {
		t.Fatalf("image/none body 应为空，实际 %d 字节", len(none[0].body))
	}
}
