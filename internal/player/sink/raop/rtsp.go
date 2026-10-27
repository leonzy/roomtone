package raop

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Response 一条 RTSP 响应。
type Response struct {
	Status int
	Header map[string]string
	Body   []byte
}

// HeaderValue 大小写无关地取头字段。
func (r *Response) HeaderValue(key string) string {
	for k, v := range r.Header {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

// Client 最小可用的 RTSP 客户端。
//
// 只实现 RAOP 需要的那几个方法：连接复用、CSeq 自增、Session 透传、
// 按 Content-Length 读 body。不处理 interleaved 数据帧——音频走独立的
// UDP 通道，控制连接上不会有 `$` 帧。
type Client struct {
	conn     net.Conn
	br       *bufio.Reader
	mu       sync.Mutex
	cseq     int
	session  string
	timeout  time.Duration
	instance string
	// DefaultHeaders 每个请求都携带的头（DACP-ID / Active-Remote 等）。
	// Do 的 headers 参数优先级更高，可覆盖。
	DefaultHeaders map[string]string
}

// Dial 建立控制连接。
func Dial(ctx context.Context, addr string, timeout time.Duration) (*Client, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("raop: 连接 %s 失败: %w", addr, err)
	}
	return &Client{
		conn: conn, br: bufio.NewReader(conn), timeout: timeout,
		instance:       fmt.Sprintf("%08X%08X", randomUint32(), randomUint32()),
		DefaultHeaders: map[string]string{},
	}, nil
}

// Session 返回 RECORD 之后设备给的会话 id。
func (c *Client) Session() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
}

// Do 发一条请求并读响应。
func (c *Client) Do(ctx context.Context, method, uri string, headers map[string]string, body []byte) (*Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.cseq++
	requestHeaders := make(map[string]string, len(headers)+2+len(c.DefaultHeaders))
	requestHeaders["User-Agent"] = "Roomtone/1.0"
	requestHeaders["Client-Instance"] = c.instance
	for k, v := range c.DefaultHeaders {
		requestHeaders[k] = v
	}
	for k, v := range headers {
		requestHeaders[k] = v
	}
	req := BuildRequest(method, uri, c.cseq, c.session, requestHeaders, body)
	deadline := time.Now().Add(c.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := c.conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if _, err := c.conn.Write(req); err != nil {
		return nil, fmt.Errorf("raop: 发送 %s 失败: %w", method, err)
	}
	resp, err := ReadResponse(c.br)
	if err != nil {
		return nil, fmt.Errorf("raop: 读取 %s 响应失败: %w", method, err)
	}
	slog.Debug("AirPlay RTSP response", "method", method, "status", resp.Status, "headers", resp.Header)
	if resp.HeaderValue("CSeq") != strconv.Itoa(c.cseq) {
		return nil, fmt.Errorf("raop: %s 响应缺少匹配的 CSeq，设备未确认请求", method)
	}
	if s := resp.HeaderValue("Session"); s != "" {
		// "Session: 1234;timeout=60" 只取 id 部分。
		c.session = strings.TrimSpace(strings.SplitN(s, ";", 2)[0])
	}
	return resp, nil
}

// Close 关闭控制连接。
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Close()
}

// BuildRequest 拼一条 RTSP 请求。
//
// 单独导出是为了 golden bytes 测试：报文顺序（CSeq 在最前、Content-Length
// 在最后、头名按字典序）必须稳定，否则每次改代码都会洗一遍测试快照。
func BuildRequest(method, uri string, cseq int, session string, headers map[string]string, body []byte) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s RTSP/1.0\r\n", method, uri)
	fmt.Fprintf(&b, "CSeq: %d\r\n", cseq)
	if session != "" {
		fmt.Fprintf(&b, "Session: %s\r\n", session)
	}
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %s\r\n", k, headers[k])
	}
	if len(body) > 0 {
		fmt.Fprintf(&b, "Content-Length: %d\r\n", len(body))
	}
	b.WriteString("\r\n")
	b.Write(body)
	return []byte(b.String())
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ReadResponse 读一条 RTSP 响应（含 body）。
func ReadResponse(br *bufio.Reader) (*Response, error) {
	line, err := br.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return nil, errors.New("raop: 空响应行")
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("raop: 无法解析状态行 %q", line)
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("raop: 状态码非法 %q", parts[1])
	}
	resp := &Response{Status: status, Header: map[string]string{}}
	for {
		h, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		h = strings.TrimRight(h, "\r\n")
		if h == "" {
			break
		}
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			continue
		}
		resp.Header[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	n := 0
	if cl := resp.HeaderValue("Content-Length"); cl != "" {
		n, _ = strconv.Atoi(cl)
	}
	if n > 0 {
		buf := make([]byte, n)
		if _, err := io.ReadFull(br, buf); err != nil {
			return nil, err
		}
		resp.Body = buf
	}
	return resp, nil
}
