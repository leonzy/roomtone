package raat

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"songloft/internal/player/sink"
)

// fakeRAATDevice 假设备端：控制 + 音频两个 listener，设备侧实现 conn 帧
// 协议应答（与客户端实现独立互验）。
type fakeRAATDevice struct {
	ctrlL  net.Listener
	audioL net.Listener

	mu         sync.Mutex
	reqs       []devRequest // 控制通道请求序列
	setupBody  json.RawMessage
	streamBody json.RawMessage
	startBody  json.RawMessage
	stopSeen   bool
	streamRID  uint32
	haveStream bool
	// 音频通道收到的帧序列（命令前缀 + 载荷）
	audioFrames []audioFrame
	// 控制通道 type=3 裸帧（封面）
	rawFrames [][]byte
	// 控制连接（服务器通知用）
	conns []net.Conn
	// setup 响应（可定制：NotSupported 错误路径）
	setupResp string
	// infoResp 可定制的 info 响应（默认内置）
	infoResp string
	// 关闭控制连接（错误路径）
	closeCtrl bool

	t *testing.T
}

type devRequest struct {
	name string
	rid  uint32
	body json.RawMessage
}

type audioFrame struct {
	cmd     byte
	payload []byte
}

func newFakeRAATDevice(t *testing.T) *fakeRAATDevice {
	t.Helper()
	ctrlL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	audioL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeRAATDevice{
		ctrlL: ctrlL, audioL: audioL,
		setupResp: `{"status":"Success","clock_port":9001,"audio_port_tcp":%d}`,
		t:         t,
	}
	go d.serveCtrl()
	go d.serveAudio()
	t.Cleanup(func() {
		ctrlL.Close()
		audioL.Close()
	})
	return d
}

func (d *fakeRAATDevice) addr() string   { return d.ctrlL.Addr().String() }
func (d *fakeRAATDevice) audioPort() int { return d.audioL.Addr().(*net.TCPAddr).Port }

func (d *fakeRAATDevice) requests() []devRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]devRequest(nil), d.reqs...)
}

func (d *fakeRAATDevice) frames() []audioFrame {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]audioFrame(nil), d.audioFrames...)
}

func (d *fakeRAATDevice) serveCtrl() {
	for {
		conn, err := d.ctrlL.Accept()
		if err != nil {
			return
		}
		go d.handleCtrl(conn)
	}
}

func (d *fakeRAATDevice) handleCtrl(conn net.Conn) {
	defer conn.Close()
	d.mu.Lock()
	d.conns = append(d.conns, conn)
	d.mu.Unlock()
	br := bufio.NewReader(conn)
	for {
		head := make([]byte, 8)
		if _, err := readFull3(br, head); err != nil {
			return
		}
		total := binary.BigEndian.Uint32(head[0:4])
		typ := binary.BigEndian.Uint32(head[4:8])
		body := make([]byte, total-8)
		if _, err := readFull3(br, body); err != nil {
			return
		}
		switch typ {
		case msgRequest:
			rid := binary.BigEndian.Uint32(body[0:4])
			var req struct {
				Request string `json:"request"`
			}
			json.Unmarshal(body[4:], &req) //nolint:errcheck // 测试
			d.mu.Lock()
			d.reqs = append(d.reqs, devRequest{name: req.Request, rid: rid, body: body[4:]})
			setupResp := d.setupResp
			closeCtrl := d.closeCtrl
			d.mu.Unlock()
			if closeCtrl {
				return // 模拟断连
			}
			d.respondCtrl(conn, rid, req.Request, setupResp)
		case msgKeepalive:
			// 忽略
		case 3:
			// type=3 裸二进制帧（封面）：带类型前缀存储（测试断言用）
			d.mu.Lock()
			d.rawFrames = append(d.rawFrames, append([]byte{3}, body...))
			d.mu.Unlock()
		}
	}
}

// respondCtrl 设备侧应答逻辑（按请求名分派）。
func (d *fakeRAATDevice) respondCtrl(conn net.Conn, rid uint32, name, setupResp string) {
	switch name {
	case "info":
		resp := d.infoResp
		if resp == "" {
			resp = `{"status":"Success","info":{"name":"fake"},"volume":{"is_supported":true},"transport":{"is_supported":true},"output":{"supported_formats":"44100/16/2, 48000/24/2, 88200/24/2"}}`
		}
		d.writeResp(conn, rid, 0x01, resp)
	case "set_client_type":
		d.writeResp(conn, rid, 0x01, `{"status":"Success"}`)
	case "setup":
		resp := strings.ReplaceAll(setupResp, "%d", fmt.Sprint(d.audioPort()))
		d.mu.Lock()
		d.setupBody = nil // 由调用方在 requests() 里取
		d.mu.Unlock()
		d.writeResp(conn, rid, 0x01, resp)
	case "stream":
		// 流式响应：先 Ready（非终），等 start 到达再 Playing（终）
		d.mu.Lock()
		d.streamRID = rid
		d.haveStream = true
		d.mu.Unlock()
		d.writeResp(conn, rid, 0x00, `{"status":"Ready"}`)
	case "start":
		// 响应 Success+time，随后补一段 Playing（终）
		d.writeResp(conn, rid, 0x01, `{"status":"Success","time":123456789}`)
		d.mu.Lock()
		srid, have := d.streamRID, d.haveStream
		d.mu.Unlock()
		if have {
			d.writeResp(conn, srid, 0x01, `{"status":"Playing"}`)
		}
	case "stop":
		d.mu.Lock()
		d.stopSeen = true
		d.mu.Unlock()
		d.writeResp(conn, rid, 0x01, `{"status":"Success"}`)
	case "update_volume":
		d.writeResp(conn, rid, 0x01, `{"status":"Success"}`)
	case "end_stream":
		d.writeResp(conn, rid, 0x01, `{"status":"Success"}`)
	default:
		d.writeResp(conn, rid, 0x01, `{"status":"Success"}`)
	}
}

func (d *fakeRAATDevice) writeResp(conn net.Conn, rid uint32, flags byte, payload string) {
	body := make([]byte, 4+1+len(payload))
	binary.BigEndian.PutUint32(body[0:4], rid)
	body[4] = flags
	copy(body[5:], payload)
	frame := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(frame[0:4], uint32(8+len(body)))
	binary.BigEndian.PutUint32(frame[4:8], msgResponse)
	frame = append(frame, body...)
	conn.Write(frame) //nolint:errcheck // 测试
}

func (d *fakeRAATDevice) serveAudio() {
	for {
		conn, err := d.audioL.Accept()
		if err != nil {
			return
		}
		go d.handleAudio(conn)
	}
}

func (d *fakeRAATDevice) handleAudio(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	for {
		head := make([]byte, 1)
		if _, err := readFull3(br, head); err != nil {
			return
		}
		cmd := head[0]
		var payload []byte
		switch cmd {
		case cmdStreamID:
			payload = make([]byte, 4)
			if _, err := readFull3(br, payload); err != nil {
				return
			}
		case cmdStreamSample:
			payload = make([]byte, 8)
			if _, err := readFull3(br, payload); err != nil {
				return
			}
		case cmdCodecData:
			// CODEC_DATA：FLAC 帧自定界（帧头 blocksize + CRC16 帧尾），
			// 长度不固定——设备端（libraatmanager）是流式解码器。fake 侧
			// 用单次非定长读（本地回环下一次 read ≈ 一次 write）。
			buf := make([]byte, 4096)
			n, err := br.Read(buf)
			if err != nil {
				return
			}
			payload = append([]byte(nil), buf[:n]...)
		default:
			// DATA：PCM 帧定长（882 采样 = 3528 字节；测试约定）
			payload = make([]byte, 882*4)
			if _, err := readFull3(br, payload); err != nil {
				return
			}
		}
		d.mu.Lock()
		d.audioFrames = append(d.audioFrames, audioFrame{cmd: cmd, payload: payload})
		d.mu.Unlock()
	}
}

func readFull3(br *bufio.Reader, buf []byte) (int, error) {
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

// --- 测试 ---

func openSink(t *testing.T, d *fakeRAATDevice) *Sink {
	t.Helper()
	s := New("dev", Options{
		Address:        d.addr(),
		SampleRate:     44100,
		Channels:       2,
		BufferSize:     2 * time.Second,
		ReadyThreshold: time.Second,
	})
	plan := sink.StreamPlan{
		Format:   sink.Format{Codec: sink.CodecL16, SampleRate: 44100, Channels: 2, Bits: 16},
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

func waitReqs(t *testing.T, d *fakeRAATDevice, n int) []devRequest {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if reqs := d.requests(); len(reqs) >= n {
			return reqs
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待超时：控制通道只收到 %d 个请求（期望 %d）: %+v", len(d.requests()), n, d.requests())
	return nil
}

// AC-B2 握手序列：info → set_client_type → setup →（音频接入）→
// STREAMID → stream。
func TestSinkHandshake(t *testing.T) {
	d := newFakeRAATDevice(t)
	openSink(t, d)

	reqs := waitReqs(t, d, 3)
	if reqs[0].name != "info" || reqs[1].name != "set_client_type" || reqs[2].name != "setup" {
		t.Fatalf("握手序列 = %v %v %v，期望 info set_client_type setup", reqs[0].name, reqs[1].name, reqs[2].name)
	}
	// set_client_type 参数
	var sct struct {
		Request    string `json:"request"`
		ClientType string `json:"client_type"`
	}
	if err := json.Unmarshal(reqs[1].body, &sct); err != nil {
		t.Fatal(err)
	}
	if sct.ClientType != "roomtone" {
		t.Fatalf("client_type = %q，期望 roomtone", sct.ClientType)
	}
	// setup format
	var setup struct {
		Request string `json:"request"`
		Format  struct {
			SampleType    string `json:"sample_type"`
			SampleRate    int    `json:"sample_rate"`
			BitsPerSample int    `json:"bits_per_sample"`
			Channels      int    `json:"channels"`
		} `json:"format"`
	}
	if err := json.Unmarshal(reqs[2].body, &setup); err != nil {
		t.Fatal(err)
	}
	if setup.Format.SampleType != "pcm" || setup.Format.SampleRate != 44100 ||
		setup.Format.BitsPerSample != 16 || setup.Format.Channels != 2 {
		t.Fatalf("setup format = %+v", setup.Format)
	}
	// stream 请求（等音频通道接入后发出）
	reqs = waitReqs(t, d, 4)
	if reqs[3].name != "stream" {
		t.Fatalf("第 4 个请求 = %q，期望 stream", reqs[3].name)
	}
	var stream struct {
		Request        string  `json:"request"`
		StreamID       int32   `json:"stream_id"`
		FirstSeq       int     `json:"first_seq"`
		NakPort        int     `json:"nak_port"`
		BufferSize     float64 `json:"buffer_size"`
		ReadyThreshold float64 `json:"ready_threshold"`
	}
	if err := json.Unmarshal(reqs[3].body, &stream); err != nil {
		t.Fatal(err)
	}
	if stream.StreamID != 1 || stream.FirstSeq != 0 || stream.NakPort != -1 {
		t.Fatalf("stream = %+v（期望 stream_id=1 first_seq=0 nak_port=-1）", stream)
	}
	if stream.BufferSize != 2.0 || stream.ReadyThreshold != 1.0 {
		t.Fatalf("stream 缓冲 = %+v（期望 buffer_size=2.0 ready_threshold=1.0）", stream)
	}
	// 音频通道：STREAMID(1)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if frames := d.frames(); len(frames) >= 1 && frames[0].cmd == cmdStreamID {
			if got := binary.BigEndian.Uint32(frames[0].payload); got != 1 {
				t.Fatalf("音频 STREAMID = %d，期望 1", got)
			}
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("音频通道未收到 STREAMID")
}

// AC-B2 起播：start 无 time 字段、有 min_offset（200ms → 2×10⁸ns）；
// Ready→EventBuffering、Playing→EventPlaying。
func TestSinkStart(t *testing.T) {
	d := newFakeRAATDevice(t)
	s := openSink(t, d)
	waitReqs(t, d, 4) // 等 stream 发出

	if err := s.Start(time.Now().Add(-time.Second)); err != nil {
		t.Fatalf("start: %v", err)
	}
	reqs := waitReqs(t, d, 5)
	if reqs[4].name != "start" {
		t.Fatalf("第 5 个请求 = %q，期望 start", reqs[4].name)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(reqs[4].body, &raw); err != nil {
		t.Fatal(err)
	}
	if _, hasTime := raw["time"]; hasTime {
		t.Fatal("P0 模式 start 不应带 time 字段")
	}
	var minOffset json.Number
	if err := json.Unmarshal(raw["min_offset"], &minOffset); err != nil {
		t.Fatalf("缺少 min_offset: %v", err)
	}
	if minOffset.String() != "200000000" {
		t.Fatalf("min_offset = %s，期望 200000000（200ms）", minOffset.String())
	}
	// 事件：EventBuffering（Ready）→ EventPlaying
	expectEvents(t, s, sink.EventBuffering, sink.EventPlaying)
}

func expectEvents(t *testing.T, s *Sink, want ...sink.EventType) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var got []sink.EventType
	for time.Now().Before(deadline) {
		for {
			select {
			case ev := <-s.Events():
				got = append(got, ev.Type)
				if len(got) >= len(want) {
					for i := range want {
						if got[i] != want[i] {
							t.Fatalf("事件序列 = %v，期望 %v", got, want)
						}
					}
					return
				}
			default:
				goto next
			}
		}
	next:
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待事件超时：收到 %v，期望 %v", got, want)
}

// AC-B2 数据：WriteFrame → STREAMSAMPLE(0)+DATA；连续帧只 DATA；
// Seek(88200) → stop→stream(stream_id=2)→start。
func TestSinkWriteAndSeek(t *testing.T) {
	d := newFakeRAATDevice(t)
	s := openSink(t, d)
	waitReqs(t, d, 4)
	if err := s.Start(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	waitReqs(t, d, 5)
	expectEvents(t, s, sink.EventBuffering, sink.EventPlaying)

	// 写帧：SampleIndex 0 → STREAMSAMPLE(0)+DATA（流内相对位置）
	frame := make([]byte, 882*4)
	if err := s.WriteFrame(sink.Frame{SampleIndex: 0, Data: frame}); err != nil {
		t.Fatal(err)
	}
	// 连续帧（882 采样后）：只 DATA
	if err := s.WriteFrame(sink.Frame{SampleIndex: 882, Data: frame}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if frames := d.frames(); len(frames) >= 4 {
			if frames[1].cmd != cmdStreamSample || binary.BigEndian.Uint64(frames[1].payload) != 0 {
				t.Fatalf("帧 1 = cmd %d payload %d，期望 STREAMSAMPLE(0)", frames[1].cmd, binary.BigEndian.Uint64(frames[1].payload))
			}
			if frames[2].cmd != cmdData || len(frames[2].payload) != 882*4 {
				t.Fatalf("帧 2 = cmd %d len %d，期望 DATA(3528)", frames[2].cmd, len(frames[2].payload))
			}
			if frames[3].cmd != cmdData {
				t.Fatalf("帧 3 = cmd %d，期望 DATA（连续无 STREAMSAMPLE）", frames[3].cmd)
			}
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	// Seek(88200)：stop → stream(stream_id=2) → start
	if err := s.Seek(88200); err != nil {
		t.Fatalf("seek: %v", err)
	}
	reqs := waitReqs(t, d, 8)
	// 请求序列：...start(5) stop(6) stream(7) start(8)
	if reqs[5].name != "stop" || reqs[6].name != "stream" || reqs[7].name != "start" {
		t.Fatalf("seek 序列 = %s %s %s，期望 stop stream start", reqs[5].name, reqs[6].name, reqs[7].name)
	}
	var stream struct {
		StreamID int32 `json:"stream_id"`
	}
	if err := json.Unmarshal(reqs[6].body, &stream); err != nil {
		t.Fatal(err)
	}
	if stream.StreamID != 2 {
		t.Fatalf("seek 后 stream_id = %d，期望 2", stream.StreamID)
	}
}

// 音量：SetVolume(0.5) → update_volume {"value":50}。
func TestSinkVolume(t *testing.T) {
	d := newFakeRAATDevice(t)
	s := openSink(t, d)
	waitReqs(t, d, 4)
	if err := s.SetVolume(0.5); err != nil {
		t.Fatal(err)
	}
	reqs := waitReqs(t, d, 5)
	if reqs[4].name != "update_volume" {
		t.Fatalf("第 5 个请求 = %q，期望 update_volume", reqs[4].name)
	}
	var vol struct {
		Value int `json:"value"`
	}
	if err := json.Unmarshal(reqs[4].body, &vol); err != nil {
		t.Fatal(err)
	}
	if vol.Value != 50 {
		t.Fatalf("update_volume value = %d，期望 50", vol.Value)
	}
}

// 位置：Start 后 PlaybackPosition() 返回 (plan.Position+流逝, true)；
// 未 Start 返回 false。
func TestSinkPlaybackPosition(t *testing.T) {
	d := newFakeRAATDevice(t)
	s := openSink(t, d)
	waitReqs(t, d, 4)

	if _, ok := s.PlaybackPosition(); ok {
		t.Fatal("未 Start 时 PlaybackPosition 应返回 false")
	}
	if err := s.Start(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	pos, ok := s.PlaybackPosition()
	if !ok {
		t.Fatal("Start 后 PlaybackPosition 应返回 true")
	}
	// plan.Position=0 + 流逝 ≈ 1s（wallStart 锚定）
	if pos < 500*time.Millisecond || pos > 2*time.Second {
		t.Fatalf("位置 = %v，期望 ≈1s", pos)
	}
}

// gapless：Continue(plan2) 不产生任何控制通道请求。
func TestSinkContinue(t *testing.T) {
	d := newFakeRAATDevice(t)
	s := openSink(t, d)
	waitReqs(t, d, 4)
	if err := s.Start(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	waitReqs(t, d, 5)
	expectEvents(t, s, sink.EventBuffering, sink.EventPlaying)

	before := len(d.requests())
	if err := s.Continue(sink.StreamPlan{SongID: 2, Title: "T2", Duration: time.Minute}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := len(d.requests()); got != before {
		t.Fatalf("Continue 产生了 %d 个新请求（应为 0）", got-before)
	}
}

// 错误路径：setup 回 NotSupported → Open 错误含 "NotSupported"；
// 控制连接关闭 → Events 出现 EventError/EventOffline。
func TestSinkErrorPaths(t *testing.T) {
	d := newFakeRAATDevice(t)
	d.mu.Lock()
	d.setupResp = `{"status":"NotSupported"}`
	d.mu.Unlock()
	s := New("dev", Options{Address: d.addr(), SampleRate: 44100, Channels: 2,
		BufferSize: 2 * time.Second, ReadyThreshold: time.Second})
	err := s.Open(context.Background(), sink.StreamPlan{
		Format: sink.Format{Codec: sink.CodecL16, SampleRate: 44100, Channels: 2, Bits: 16},
	})
	if err == nil || !strings.Contains(err.Error(), "NotSupported") {
		t.Fatalf("setup NotSupported 应报错: %v", err)
	}
	s.Close()

	// 控制连接关闭 → EventError/EventOffline
	d2 := newFakeRAATDevice(t)
	s2 := New("dev", Options{Address: d2.addr(), SampleRate: 44100, Channels: 2,
		BufferSize: 2 * time.Second, ReadyThreshold: time.Second})
	if err := s2.Open(context.Background(), sink.StreamPlan{
		Format: sink.Format{Codec: sink.CodecL16, SampleRate: 44100, Channels: 2, Bits: 16},
	}); err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	waitReqs(t, d2, 4)
	d2.mu.Lock()
	d2.closeCtrl = true
	d2.mu.Unlock()
	// 触发一个新请求让设备端断连（handleCtrl return）
	s2.SetVolume(0.5) //nolint:errcheck // 错误路径测试
	deadline := time.Now().Add(3 * time.Second)
	sawErr := false
	for time.Now().Before(deadline) {
		select {
		case ev := <-s2.Events():
			if ev.Type == sink.EventError || ev.Type == sink.EventOffline {
				sawErr = true
			}
		default:
			time.Sleep(2 * time.Millisecond)
		}
		if sawErr {
			break
		}
	}
	if !sawErr {
		t.Fatal("控制连接关闭后应出现 EventError/EventOffline")
	}
}

func TestSinkStartHonorsFutureDeadline(t *testing.T) {
	d := newFakeRAATDevice(t)
	s := openSink(t, d)
	at := time.Now().Add(2 * time.Second)
	if err := s.Start(at); err != nil {
		t.Fatal(err)
	}
	reqs := waitReqs(t, d, 5)
	var body struct {
		Offset int64 `json:"min_offset"`
	}
	if err := json.Unmarshal(reqs[4].body, &body); err != nil {
		t.Fatal(err)
	}
	if time.Duration(body.Offset) < 1500*time.Millisecond || time.Duration(body.Offset) > 2*time.Second {
		t.Fatalf("future start ignored: offset=%s", time.Duration(body.Offset))
	}
}
