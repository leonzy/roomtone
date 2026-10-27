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
)

// --- 假设备端（内存 listener，实现协议设备侧）---

type fakeDevice struct {
	ln net.Listener

	mu    sync.Mutex
	conns []*fakeDevConn
}

type fakeDevConn struct {
	conn net.Conn
	br   *bufio.Reader
	// 收到的请求记录
	mu      sync.Mutex
	reqs    []devReq
	keepN   int
	unknown []uint32
}

type devReq struct {
	rid  uint32
	body json.RawMessage
}

func newFakeDevice(t *testing.T) *fakeDevice {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	d := &fakeDevice{ln: ln}
	go d.serve()
	t.Cleanup(func() { ln.Close() })
	return d
}

func (d *fakeDevice) addr() string { return d.ln.Addr().String() }

func (d *fakeDevice) serve() {
	for {
		conn, err := d.ln.Accept()
		if err != nil {
			return
		}
		fc := &fakeDevConn{conn: conn, br: bufio.NewReader(conn)}
		d.mu.Lock()
		d.conns = append(d.conns, fc)
		d.mu.Unlock()
		go fc.readLoop()
	}
}

func (d *fakeDevice) lastConn() *fakeDevConn {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.conns) == 0 {
		return nil
	}
	return d.conns[len(d.conns)-1]
}

// readLoop 设备侧：读帧，REQUEST 记录并回调（handler 决定如何应答）。
func (f *fakeDevConn) readLoop() {
	for {
		head := make([]byte, 8)
		if _, err := readFull(f.br, head); err != nil {
			return
		}
		total := binary.BigEndian.Uint32(head[0:4])
		typ := binary.BigEndian.Uint32(head[4:8])
		body := make([]byte, total-8)
		if _, err := readFull(f.br, body); err != nil {
			return
		}
		switch typ {
		case msgRequest:
			rid := binary.BigEndian.Uint32(body[0:4])
			f.mu.Lock()
			f.reqs = append(f.reqs, devReq{rid: rid, body: json.RawMessage(body[4:])})
			f.mu.Unlock()
		case msgKeepalive:
			f.mu.Lock()
			f.keepN++
			f.mu.Unlock()
		default:
			f.mu.Lock()
			f.unknown = append(f.unknown, typ)
			f.mu.Unlock()
		}
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

// respond 设备侧回 RESPONSE 帧。
func (f *fakeDevConn) respond(rid uint32, flags byte, payload string) {
	body := make([]byte, 4+1+len(payload))
	binary.BigEndian.PutUint32(body[0:4], rid)
	body[4] = flags
	copy(body[5:], payload)
	total := 8 + len(body)
	frame := make([]byte, 8, total)
	binary.BigEndian.PutUint32(frame[0:4], uint32(total))
	binary.BigEndian.PutUint32(frame[4:8], msgResponse)
	frame = append(frame, body...)
	f.conn.Write(frame) //nolint:errcheck // 测试
}

// sendServerReq 设备侧主动发 type=2 服务器请求。
func (f *fakeDevConn) sendServerReq(rid uint32, flags byte, payload string) {
	body := make([]byte, 4+1+len(payload))
	binary.BigEndian.PutUint32(body[0:4], rid)
	body[4] = flags
	copy(body[5:], payload)
	total := 8 + len(body)
	frame := make([]byte, 8, total)
	binary.BigEndian.PutUint32(frame[0:4], uint32(total))
	binary.BigEndian.PutUint32(frame[4:8], msgServerReq)
	frame = append(frame, body...)
	f.conn.Write(frame) //nolint:errcheck // 测试
}

func (f *fakeDevConn) requests() []devReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]devReq(nil), f.reqs...)
}

func (f *fakeDevConn) keepalives() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.keepN
}

// --- 测试 ---

func dialFake(t *testing.T) (*Conn, *fakeDevConn) {
	t.Helper()
	d := newFakeDevice(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	c, err := DialControl(ctx, d.addr())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	// 等连接建立
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if d.lastConn() != nil {
			return c, d.lastConn()
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("设备端未收到连接")
	return nil, nil
}

// 帧编解码 golden：writeFrame 输出 [4B BE len][4B BE type][body]。
func TestConnFrameGolden(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	c := NewConn(c1)
	defer c.Close()

	// net.Pipe 的 Write 阻塞到对端读走数据——读必须并发。
	type res struct {
		buf []byte
		err error
	}
	ch := make(chan res, 1)
	go func() {
		buf := make([]byte, 10)
		_, err := readFull(bufio.NewReader(c2), buf)
		ch <- res{buf, err}
	}()
	if err := c.writeFrame(msgRequest, []byte{0xAA, 0xBB}); err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	want := []byte{0, 0, 0, 10, 0x80, 0, 0, 1, 0xAA, 0xBB}
	if string(r.buf) != string(want) {
		t.Fatalf("帧不符:\n got=%x\nwant=%x", r.buf, want)
	}

	// 超长：total > 1MB → readLoop 断连（failAll 语义）。
	// 注意方向：readLoop 读 c1，故从 c2 写入（c2→c1）；
	// 必须写完整 8 字节头（ReadFull(8) 才能返回做长度检查）。
	long := make([]byte, 8)
	binary.BigEndian.PutUint32(long[0:4], maxFrameSize+100)
	binary.BigEndian.PutUint32(long[4:8], msgRequest)
	c2.Write(long) //nolint:errcheck // 测试
	// readLoop 应断连（closed 通道关闭）
	select {
	case <-c.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("超长消息应断连")
	}
}

// 请求关联：fake 设备端读到 REQUEST（断言 JSON {"request":"info"} 且 rid
// 递增），回 RESPONSE → Request 的 onReply 收到 (raw, final=true) 且
// Request 返回 nil。
func TestConnRequestReply(t *testing.T) {
	c, dev := dialFake(t)

	// 设备侧应答协程：收到请求立即回 Success
	go func() {
		for {
			reqs := dev.requests()
			if len(reqs) == 0 {
				time.Sleep(time.Millisecond)
				continue
			}
			dev.respond(reqs[0].rid, 0x01, `{"status":"Success","info":{"x":1}}`)
			return
		}
	}()

	var gotRaw json.RawMessage
	var gotFinal bool
	raw, err := c.Request(context.Background(), "info", nil, func(r json.RawMessage, final bool) error {
		gotRaw, gotFinal = r, final
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"status":"Success","info":{"x":1}}` {
		t.Fatalf("raw = %s", raw)
	}
	if !gotFinal || string(gotRaw) != `{"status":"Success","info":{"x":1}}` {
		t.Fatalf("onReply = (%s, %v)", gotRaw, gotFinal)
	}
	// 请求 JSON 形态与 rid
	reqs := dev.requests()
	if len(reqs) != 1 {
		t.Fatalf("设备收到 %d 个请求", len(reqs))
	}
	if string(reqs[0].body) != `{"request":"info"}` {
		t.Fatalf("请求 JSON = %s", reqs[0].body)
	}
	if reqs[0].rid == 0 {
		t.Fatal("rid 应非零")
	}

	// 第二个请求 rid 递增
	go func() {
		for {
			reqs := dev.requests()
			if len(reqs) < 2 {
				time.Sleep(time.Millisecond)
				continue
			}
			dev.respond(reqs[1].rid, 0x01, `{"status":"Success"}`)
			return
		}
	}()
	if _, err := c.Request(context.Background(), "set_client_type", map[string]any{"client_type": "roomtone"}, nil); err != nil {
		t.Fatal(err)
	}
	reqs = dev.requests()
	if len(reqs) != 2 || reqs[1].rid <= reqs[0].rid {
		t.Fatalf("rid 应递增: %v", reqs)
	}
	if !strings.Contains(string(reqs[1].body), `"client_type":"roomtone"`) {
		t.Fatalf("参数应合并进请求: %s", reqs[1].body)
	}
}

// 流式响应：设备先回 [rid][0x00]{"status":"Ready"} 再回
// [rid][0x01]{"status":"Playing"} → onReply 被调两次，第一次 final=false。
func TestConnStreamingResponse(t *testing.T) {
	c, dev := dialFake(t)

	go func() {
		for {
			reqs := dev.requests()
			if len(reqs) == 0 {
				time.Sleep(time.Millisecond)
				continue
			}
			rid := reqs[0].rid
			dev.respond(rid, 0x00, `{"status":"Ready"}`)
			dev.respond(rid, 0x01, `{"status":"Playing"}`)
			return
		}
	}()

	var mu sync.Mutex
	var calls []struct {
		raw   string
		final bool
	}
	_, err := c.Request(context.Background(), "stream", nil, func(r json.RawMessage, final bool) error {
		mu.Lock()
		calls = append(calls, struct {
			raw   string
			final bool
		}{string(r), final})
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 {
		t.Fatalf("onReply 应被调 2 次，实际 %d: %+v", len(calls), calls)
	}
	if calls[0].raw != `{"status":"Ready"}` || calls[0].final {
		t.Fatalf("首段 = %+v", calls[0])
	}
	if calls[1].raw != `{"status":"Playing"}` || !calls[1].final {
		t.Fatalf("末段 = %+v", calls[1])
	}
}

// 服务器主动请求：设备发 type=2 → OnServerRequest 收到 JSON。
func TestConnServerRequest(t *testing.T) {
	c, dev := dialFake(t)

	got := make(chan json.RawMessage, 1)
	c.OnServerRequest = func(raw json.RawMessage) { got <- raw }

	dev.sendServerReq(100, 0x01, `{"control":{"button":"pause"}}`)
	select {
	case raw := <-got:
		if string(raw) != `{"control":{"button":"pause"}}` {
			t.Fatalf("OnServerRequest = %s", raw)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未收到服务器请求")
	}
}

// keepalive：StartKeepalive(10ms) 后 100ms 内收到 ≥5 个空帧。
func TestConnKeepalive(t *testing.T) {
	c, dev := dialFake(t)
	c.StartKeepalive(10 * time.Millisecond)
	time.Sleep(100 * time.Millisecond)
	if n := dev.keepalives(); n < 5 {
		t.Fatalf("keepalive 只收到 %d 个（期望 ≥5）", n)
	}
}

// 错误状态：设备回 {"status":"NotSupported"} → RequestSimple 返回
// "raat: info 返回 NotSupported"。
func TestConnErrorStatus(t *testing.T) {
	c, dev := dialFake(t)
	go func() {
		for {
			reqs := dev.requests()
			if len(reqs) == 0 {
				time.Sleep(time.Millisecond)
				continue
			}
			dev.respond(reqs[0].rid, 0x01, `{"status":"NotSupported"}`)
			return
		}
	}()
	err := c.RequestSimple(context.Background(), "info", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "NotSupported") {
		t.Fatalf("错误 = %v，期望含 NotSupported", err)
	}
	// 错误信息格式：raat: info 返回 NotSupported
	if err.Error() != "raat: info 返回 NotSupported" {
		t.Fatalf("错误信息 = %q", err.Error())
	}
}

// WriteRaw：type=3 裸帧（封面路径）。
func TestConnWriteRaw(t *testing.T) {
	c, dev := dialFake(t)
	if err := c.WriteRaw(msgArtwork, []byte{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	// 设备侧 unknown 帧记录（type=3 不是 REQUEST/KEEPALIVE）
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		dev.mu.Lock()
		n := len(dev.unknown)
		dev.mu.Unlock()
		if n >= 1 {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("设备未收到 type=3 裸帧")
}

var _ = fmt.Sprintf
