package raat

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"songloft/internal/player/sink"
)

// screenEnv 屏显/反控测试环境：fakeRAATDevice + 断言助手。
type screenEnv struct {
	d *fakeRAATDevice
	s *Sink
}

func newScreenEnv(t *testing.T) *screenEnv {
	t.Helper()
	d := newFakeRAATDevice(t)
	var mu sync.Mutex
	var lastCtrl struct {
		cmd   string
		value float64
	}
	s := New("dev", Options{
		Address:        d.addr(),
		SampleRate:     44100,
		Channels:       2,
		BufferSize:     2 * time.Second,
		ReadyThreshold: time.Second,
		OnControl: func(cmd string, v float64) error {
			mu.Lock()
			lastCtrl.cmd, lastCtrl.value = cmd, v
			mu.Unlock()
			return nil
		},
	})
	if err := s.Open(context.Background(), sink.StreamPlan{
		Format: sink.Format{Codec: sink.CodecL16, SampleRate: 44100, Channels: 2, Bits: 16},
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Start(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitReqs(t, d, 5) // info set_client_type setup stream start
	return &screenEnv{d: d, s: s}
}

func (e *screenEnv) control() (string, float64) {
	e.d.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		// OnControl 经 DeviceControl 映射；这里直接断言收到的原始按钮
		// （screen.go 把 button 映射为 DeviceControl 命令）
		time.Sleep(2 * time.Millisecond)
	}
	return "", 0
}

// findReq 找指定名称的最新请求。
func (e *screenEnv) findReq(t *testing.T, name string, after int) (int, devRequest) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		reqs := e.d.requests()
		for i := after; i < len(reqs); i++ {
			if reqs[i].name == name {
				return i, reqs[i]
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时（after=%d，共 %d 个请求）", name, after, len(e.d.requests()))
	return 0, devRequest{}
}

// UpdateMetadata → update_transport（quiet:true + status.now_playing + state）。
func TestScreenUpdateMetadata(t *testing.T) {
	e := newScreenEnv(t)
	if err := e.s.UpdateMetadata(sink.NowPlaying{Title: "T", Artist: "A", Album: "Al"}); err != nil {
		t.Fatal(err)
	}
	_, req := e.findReq(t, "update_transport", 5)
	var body struct {
		Quiet  bool `json:"quiet"`
		Status struct {
			State      string `json:"state"`
			NowPlaying struct {
				Title  string `json:"title"`
				Artist string `json:"artist"`
				Album  string `json:"album"`
			} `json:"now_playing"`
		} `json:"status"`
	}
	if err := json.Unmarshal(req.body, &body); err != nil {
		t.Fatal(err)
	}
	if !body.Quiet {
		t.Fatal("update_transport 应带 quiet:true")
	}
	if body.Status.NowPlaying.Title != "T" || body.Status.NowPlaying.Artist != "A" || body.Status.NowPlaying.Album != "Al" {
		t.Fatalf("now_playing 不符: %+v", body.Status.NowPlaying)
	}
	if body.Status.State != "playing" {
		t.Fatalf("state = %q，期望 playing（sink 当前态）", body.Status.State)
	}
}

// UpdateProgress → update_transport 单请求全量推送（state+seek+now_playing.length）。
func TestScreenUpdateProgress(t *testing.T) {
	e := newScreenEnv(t)
	if err := e.s.UpdateProgress(30*time.Second, 180*time.Second); err != nil {
		t.Fatal(err)
	}
	_, req := e.findReq(t, "update_transport", 5)
	var body struct {
		Status struct {
			State      string `json:"state"`
			Seek       int    `json:"seek"`
			NowPlaying struct {
				Length int `json:"length"`
			} `json:"now_playing"`
			IsSeekAllowed bool `json:"is_seek_allowed"`
		} `json:"status"`
	}
	if err := json.Unmarshal(req.body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Status.Seek != 30 {
		t.Fatalf("seek = %d，期望 30", body.Status.Seek)
	}
	if body.Status.NowPlaying.Length != 180 {
		t.Fatalf("length = %d，期望 180", body.Status.NowPlaying.Length)
	}
	if !body.Status.IsSeekAllowed {
		t.Fatal("is_seek_allowed 应为 true")
	}
	if body.Status.State != "playing" {
		t.Fatalf("state = %q", body.Status.State)
	}
}

// UpdateArtwork → 先 type=3 裸二进制帧（JPEG 字节），再 update_artwork
// {mimetype:"image/jpeg"}；PNG 输入被转码为 JPEG；空路径 → mimetype=null 清除。
func TestScreenUpdateArtwork(t *testing.T) {
	e := newScreenEnv(t)
	// 造 JPEG 与 PNG 文件
	dir := t.TempDir()
	jpgPath := filepath.Join(dir, "c.jpg")
	var jpgBytes []byte
	{
		img := image.NewRGBA(image.Rect(0, 0, 4, 4))
		img.Set(0, 0, color.RGBA{R: 255, A: 255})
		var buf fileBuf
		if err := jpeg.Encode(&buf, img, nil); err != nil {
			t.Fatal(err)
		}
		jpgBytes = buf.b
		if err := os.WriteFile(jpgPath, jpgBytes, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pngPath := filepath.Join(dir, "c.png")
	{
		img := image.NewRGBA(image.Rect(0, 0, 4, 4))
		img.Set(1, 1, color.RGBA{B: 255, A: 255})
		var buf fileBuf
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(pngPath, buf.b, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 1) JPEG 原样透传
	if err := e.s.UpdateArtwork(jpgPath); err != nil {
		t.Fatal(err)
	}
	// type=3 裸帧：等音频通道收到（screen 走控制通道 WriteRaw）
	deadline := time.Now().Add(3 * time.Second)
	var rawFrame []byte
	for time.Now().Before(deadline) {
		e.d.mu.Lock()
		for _, f := range e.d.audioFrames {
			_ = f
		}
		e.d.mu.Unlock()
		if f, ok := e.d.takeRawFrame(); ok {
			rawFrame = f
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if rawFrame == nil {
		t.Fatal("未收到 type=3 裸帧")
	}
	if rawFrame[0] != 3 {
		t.Fatalf("裸帧类型 = %d，期望 3", rawFrame[0])
	}
	if string(rawFrame[1:]) != string(jpgBytes) {
		t.Fatal("裸帧 body 应为 JPEG 原始字节")
	}
	i1, req := e.findReq(t, "update_artwork", 5)
	var body struct {
		Mimetype string `json:"mimetype"`
	}
	if err := json.Unmarshal(req.body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Mimetype != "image/jpeg" {
		t.Fatalf("mimetype = %q，期望 image/jpeg", body.Mimetype)
	}

	// 2) PNG 输入被转码为 JPEG
	if err := e.s.UpdateArtwork(pngPath); err != nil {
		t.Fatal(err)
	}
	i2, _ := e.findReq(t, "update_artwork", i1+1) // 第二个（PNG 转码）
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f, ok := e.d.takeRawFrame(); ok {
			if f[0] != 3 {
				t.Fatalf("裸帧类型 = %d", f[0])
			}
			if string(f[1:]) == string(jpgBytes) {
				t.Fatal("PNG 应被转码（字节应不同于原 JPEG）")
			}
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	// 3) 空路径 → mimetype=null 清除（无裸帧）
	before := e.d.rawFrameCount()
	if err := e.s.UpdateArtwork(""); err != nil {
		t.Fatal(err)
	}
	_, req = e.findReq(t, "update_artwork", i2+1) // 第三个（清除）
	// 注意：json.Unmarshal 对 JSON null 是 no-op（不清零已有字段），
	// 这里直接断言原始 body 含 "mimetype":null。
	if !bytesContainsStr(string(req.body), `"mimetype":null`) {
		t.Fatalf("清除时 mimetype 应为 null，实际 body = %s", req.body)
	}
	if got := e.d.rawFrameCount(); got != before {
		t.Fatal("清除路径不应发裸帧")
	}
}

// 反控：设备主动发 type=2 {"control":{"button":"pause"}} → OnControl；
// {"control":{"seek":{...}}} → OnControl("seek", seconds)。
func TestScreenRemoteControl(t *testing.T) {
	e := newScreenEnv(t)

	// 等 subscribe_transport 发出（Open 时 transport 支持则订阅）
	e.findReq(t, "subscribe_transport", 4)

	// 设备发按钮通知
	e.d.mu.Lock()
	conn := e.d.ctrlConns()[0]
	e.d.mu.Unlock()
	sendServerNotification(conn, `{"control":{"button":"pause"}}`)

	// 等 OnControl 被调（经 screen.go 映射 pause → DeviceControl "pause"）
	deadline := time.Now().Add(2 * time.Second)
	var gotCmd string
	for time.Now().Before(deadline) {
		e.s.ctrlMu.Lock()
		if e.s.lastControlCmd != "" {
			gotCmd = e.s.lastControlCmd
			e.s.ctrlMu.Unlock()
			break
		}
		e.s.ctrlMu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
	if gotCmd != "pause" {
		t.Fatalf("OnControl 命令 = %q，期望 pause", gotCmd)
	}

	// seek 通知
	sendServerNotification(conn, `{"control":{"seek":{"mode":"absolute","seconds":42}}}`)
	deadline = time.Now().Add(2 * time.Second)
	var gotSeek string
	var gotVal float64
	for time.Now().Before(deadline) {
		e.s.ctrlMu.Lock()
		if e.s.lastControlCmd == "seek" {
			gotSeek, gotVal = e.s.lastControlCmd, e.s.lastControlVal
			e.s.ctrlMu.Unlock()
			break
		}
		e.s.ctrlMu.Unlock()
		time.Sleep(2 * time.Millisecond)
	}
	if gotSeek != "seek" || gotVal != 42 {
		t.Fatalf("seek 通知 = (%q,%v)，期望 (seek,42)", gotSeek, gotVal)
	}
}

// --- 助手 ---

type fileBuf struct{ b []byte }

func (f *fileBuf) Write(p []byte) (int, error) {
	f.b = append(f.b, p...)
	return len(p), nil
}

// takeRawFrame 取一条控制通道 type=3 裸帧（fakeRAATDevice 扩展）。
func (d *fakeRAATDevice) takeRawFrame() ([]byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.rawFrames) == 0 {
		return nil, false
	}
	f := d.rawFrames[0]
	d.rawFrames = d.rawFrames[1:]
	return f, true
}

func (d *fakeRAATDevice) rawFrameCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.rawFrames)
}

func (d *fakeRAATDevice) ctrlConns() []net.Conn {
	return d.conns
}

// sendServerNotification 设备侧发 type=2 服务器请求通知。
func sendServerNotification(conn net.Conn, payload string) {
	body := make([]byte, 4+1+len(payload))
	binary.BigEndian.PutUint32(body[0:4], 999)
	body[4] = 0x01
	copy(body[5:], payload)
	frame := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(frame[0:4], uint32(8+len(body)))
	binary.BigEndian.PutUint32(frame[4:8], 2) // 服务器请求
	frame = append(frame, body...)
	conn.Write(frame) //nolint:errcheck // 测试
}

func bytesContainsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestDeviceControlCanRequestOnSameConnection(t *testing.T) {
	d := newFakeRAATDevice(t)
	done := make(chan error, 1)
	var s *Sink
	s = New("dev", Options{Address: d.addr(), OnControl: func(cmd string, v float64) error {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		err := s.ctrl.RequestSimple(ctx, "stop", nil, nil)
		done <- err
		return err
	}})
	if err := s.Open(context.Background(), sink.StreamPlan{Format: sink.Format{SampleRate: 44100, Channels: 2, Bits: 16}}); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d.mu.Lock()
	conn := d.ctrlConns()[0]
	d.mu.Unlock()
	sendServerNotification(conn, `{"control":{"button":"pause"}}`)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("control blocked its own response reader: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("control did not complete")
	}
}
