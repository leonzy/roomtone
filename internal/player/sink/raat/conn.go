package raat

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

// 控制通道常量（规格 §②）。
const (
	msgRequest      = 0x80000001
	msgResponse     = 0x80000002
	msgKeepalive    = 0x80000003
	msgCoreResponse = 0x80000004 // Core 响应设备主动发起的请求
	msgServerReq    = 2          // 设备→Core 主动请求（subscribe 通知等）
	msgArtwork      = 3          // Core→设备 裸二进制封面

	maxFrameSize = 1 << 20 // 超长消息断连
)

// Conn RAAT 控制通道：Core 侧协议客户端（设备端是 TCP server）。
//
// 帧格式：[4B BE total_length（含头 8B）][4B BE message_type][body]。
// REQUEST body=[4B rid][JSON]；RESPONSE body=[4B rid][1B flags(bit0=is_final)]
// [JSON]；KEEPALIVE 空帧每 2000ms。
type Conn struct {
	conn net.Conn
	mu   sync.Mutex // 写串行化

	nextID  uint32
	pending sync.Map // rid -> *call
	// OnServerRequest type=2 通知（subscribe 类）回调。
	OnServerRequest func(raw json.RawMessage)
	// OnCoreResponse type=0x80000004（Core 响应设备请求）回调。
	OnCoreResponse func(raw json.RawMessage)

	closed chan struct{}
	once   sync.Once
}

type call struct {
	onReply func(raw json.RawMessage, final bool) error
	done    chan struct{}
	err     error
	last    json.RawMessage
}

// ErrClosed 连接已关闭。
var ErrClosed = errors.New("raat: 连接已关闭")

// DialControl 连接设备控制通道并启动读循环。
func DialControl(ctx context.Context, addr string) (*Conn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("raat: 连接 %s 失败: %w", addr, err)
	}
	c := &Conn{conn: conn, closed: make(chan struct{})}
	go c.readLoop()
	return c, nil
}

// NewConn 包装既有连接（测试用）。
func NewConn(conn net.Conn) *Conn {
	c := &Conn{conn: conn, closed: make(chan struct{})}
	go c.readLoop()
	return c
}

// Close 关闭连接。
func (c *Conn) Close() error {
	c.once.Do(func() { close(c.closed) })
	err := c.conn.Close()
	// 唤醒所有等待中的调用
	c.pending.Range(func(_, v any) bool {
		cl := v.(*call)
		select {
		case <-cl.done:
		default:
			cl.err = ErrClosed
			close(cl.done)
		}
		return true
	})
	return err
}

// writeFrame 写一帧：[4B BE total][4B BE type][body]。
func (c *Conn) writeFrame(t uint32, body []byte) error {
	total := 8 + len(body)
	buf := make([]byte, 8, total)
	binary.BigEndian.PutUint32(buf[0:4], uint32(total))
	binary.BigEndian.PutUint32(buf[4:8], t)
	buf = append(buf, body...)
	_, err := c.conn.Write(buf)
	return err
}

// Request 发起请求。args 为请求参数（不含 "request" 键）；onReply 收到
// 每段响应（流式响应多次回调，final 标记末段）；onReply 为 nil 时只等
// 首段。返回值：onReply 为 nil 时返回首段 raw；否则返回末段后 nil。
func (c *Conn) Request(ctx context.Context, name string, args map[string]any, onReply func(raw json.RawMessage, final bool) error) (json.RawMessage, error) {
	if args == nil {
		args = map[string]any{}
	}
	args["request"] = name
	payload, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.nextID++
	rid := c.nextID
	c.mu.Unlock()

	body := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(body[0:4], rid)
	copy(body[4:], payload)

	cl := &call{onReply: onReply, done: make(chan struct{})}
	c.pending.Store(rid, cl)
	defer c.pending.Delete(rid)

	if err := c.writeFrame(msgRequest, body); err != nil {
		return nil, fmt.Errorf("raat: 发送 %s 失败: %w", name, err)
	}

	// 等待：末段响应 / ctx 结束 / 连接关闭
	select {
	case <-cl.done:
		if cl.err != nil {
			return nil, fmt.Errorf("raat: %s: %w", name, cl.err)
		}
		return cl.last, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("raat: %s: %w", name, ctx.Err())
	case <-c.closed:
		return nil, fmt.Errorf("raat: %s: %w", name, ErrClosed)
	}
}

// RequestSimple 单段响应：首响应 status != "Success" → 错误；JSON 反序列化到 out。
func (c *Conn) RequestSimple(ctx context.Context, name string, args map[string]any, out any) error {
	raw, err := c.Request(ctx, name, args, nil)
	if err != nil {
		return err
	}
	var head struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return fmt.Errorf("raat: %s 响应解析失败: %w", name, err)
	}
	if head.Status != "Success" {
		return fmt.Errorf("raat: %s 返回 %s", name, head.Status)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("raat: %s 响应解析失败: %w", name, err)
		}
	}
	return nil
}

// WriteRaw 写裸帧（type=3 封面等）。
func (c *Conn) WriteRaw(t uint32, body []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeFrame(t, body)
}

// StartKeepalive 每 interval 发一个 KEEPALIVE 空帧（2000ms）。
func (c *Conn) StartKeepalive(interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-c.closed:
				return
			case <-ticker.C:
				c.mu.Lock()
				err := c.writeFrame(msgKeepalive, nil)
				c.mu.Unlock()
				if err != nil {
					return // 写失败即停
				}
			}
		}
	}()
}

// readLoop 读循环：帧 → 按 type 分发。
func (c *Conn) readLoop() {
	br := bufio.NewReader(c.conn)
	for {
		select {
		case <-c.closed:
			return
		default:
		}
		head := make([]byte, 8)
		if _, err := io.ReadFull(br, head); err != nil {
			c.failAll(err)
			return
		}
		total := binary.BigEndian.Uint32(head[0:4])
		if total < 8 || int(total) > maxFrameSize {
			slog.Warn("raat: 消息长度越界，断连", "total", total)
			c.failAll(fmt.Errorf("raat: 消息长度 %d 越界", total))
			return
		}
		typ := binary.BigEndian.Uint32(head[4:8])
		body := make([]byte, total-8)
		if _, err := io.ReadFull(br, body); err != nil {
			c.failAll(err)
			return
		}
		c.dispatch(typ, body)
	}
}

func (c *Conn) dispatch(typ uint32, body []byte) {
	switch typ {
	case msgResponse, msgCoreResponse:
		if len(body) < 5 {
			return
		}
		rid := binary.BigEndian.Uint32(body[0:4])
		flags := body[4]
		raw := json.RawMessage(body[5:])
		final := flags&1 != 0
		if typ == msgCoreResponse {
			if c.OnCoreResponse != nil {
				c.OnCoreResponse(raw)
			}
			return
		}
		if cl, ok := c.pending.Load(rid); ok {
			cc := cl.(*call)
			if cc.onReply != nil {
				if err := cc.onReply(raw, final); err != nil {
					cc.err = err
				}
				cc.last = raw
				if final {
					close(cc.done)
				}
			} else {
				// 无回调：首段即完成
				cc.last = raw
				close(cc.done)
			}
		}
	case msgServerReq:
		if len(body) < 5 {
			return
		}
		raw := json.RawMessage(body[5:])
		if c.OnServerRequest != nil {
			c.OnServerRequest(raw)
		}
	case msgArtwork:
		// Core→设备方向，控制通道客户端不处理
	default:
		slog.Debug("raat: got unknown ll message type %x", "type", typ)
	}
}

func (c *Conn) failAll(err error) {
	c.once.Do(func() { close(c.closed) })
	c.pending.Range(func(_, v any) bool {
		cl := v.(*call)
		select {
		case <-cl.done:
		default:
			cl.err = err
			close(cl.done)
		}
		return true
	})
}
