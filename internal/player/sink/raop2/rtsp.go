package raop2

import (
	"bufio"
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Response 一条 RTSP 响应（与 raop.Response 同形态，V2 独立定义）。
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

// RTSPClient AirPlay 2 的 RTSP 客户端（RTSPClientV2.cs 的 Go 对应物）。
//
// 与 V1 的差异：请求行用路径（POST /pair-setup RTSP/1.0）而非 rtsp:// URI；
// 配对完成后可启用加密（InitEncryption，A2P-4 的 EncryptionContext），
// 之后所有请求体密文分块、响应体解密。
type RTSPClient struct {
	conn     net.Conn
	br       *bufio.Reader
	mu       sync.Mutex
	cseq     int
	session  string
	timeout  time.Duration
	instance string
	// DefaultHeaders 每个请求都携带的头（DACP-ID / Active-Remote 等）。
	DefaultHeaders map[string]string
	// enc 加密上下文（InitEncryption 后非 nil）。
	enc *EncryptionContext
}

// Dial 建立控制连接。
func Dial(ctx context.Context, addr string, timeout time.Duration) (*RTSPClient, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("raop2: 连接 %s 失败: %w", addr, err)
	}
	return &RTSPClient{
		conn: conn, br: bufio.NewReader(conn), timeout: timeout,
		instance:       fmt.Sprintf("%016X%016X", randomUint64(), randomUint64()),
		DefaultHeaders: map[string]string{},
	}, nil
}

// Conn 底层连接（event 通道等需要地址信息时用）。
func (c *RTSPClient) Conn() net.Conn { return c.conn }

// Session 会话 id（RECORD 之后）。
func (c *RTSPClient) Session() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.session
}

// InitEncryption 启用加密 RTSP（配对完成后调用，对齐
// RTSPClientV2.InitEncryption: ChaChaPoly + Control 通道）。
func (c *RTSPClient) InitEncryption(encType EncryptionType, sessionKey []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.enc = NewEncryptionContext(encType, ChannelControl, sessionKey)
	slog.Info("AirPlay2 加密 RTSP 已启用", "type", encType)
}

// EncryptionEnabled 是否已启用加密。
func (c *RTSPClient) EncryptionEnabled() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enc != nil
}

// Do 发一条请求并读响应。启用加密后请求体加密、响应体解密。
func (c *RTSPClient) Do(ctx context.Context, method, path string, headers map[string]string, body []byte) (*Response, error) {
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
	wire := body
	if c.enc != nil && body != nil {
		wire = c.enc.Encrypt(body)
	}
	req := BuildRequest(method, path, c.cseq, c.session, requestHeaders, wire)
	deadline := time.Now().Add(c.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := c.conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	if _, err := c.conn.Write(req); err != nil {
		return nil, fmt.Errorf("raop2: 发送 %s 失败: %w", method, err)
	}
	resp, err := ReadResponse(c.br)
	if err != nil {
		return nil, fmt.Errorf("raop2: 读取 %s 响应失败: %w", method, err)
	}
	if resp.HeaderValue("CSeq") != strconv.Itoa(c.cseq) {
		return nil, fmt.Errorf("raop2: %s 响应缺少匹配的 CSeq，设备未确认请求", method)
	}
	if s := resp.HeaderValue("Session"); s != "" {
		c.session = strings.TrimSpace(strings.SplitN(s, ";", 2)[0])
	}
	if c.enc != nil && len(resp.Body) > 0 {
		plain, _, err := c.enc.Decrypt(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("raop2: %s 响应体解密失败: %w", method, err)
		}
		resp.Body = plain
	}
	return resp, nil
}

// Close 关闭连接。
func (c *RTSPClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.Close()
}

// BuildRequest 拼一条 RTSP 请求（请求行用路径；头按字典序稳定输出，
// CSeq 在头之后——对齐 RTSPClientV2 的写出顺序：请求行、业务头、CSeq、
// Content-Length）。
func BuildRequest(method, path string, cseq int, session string, headers map[string]string, body []byte) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s RTSP/1.0\r\n", method, path)
	keys := make([]string, 0, len(headers))
	for k := range headers {
		keys = append(keys, k)
	}
	sortStrings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s: %s\r\n", k, headers[k])
	}
	fmt.Fprintf(&b, "CSeq: %d\r\n", cseq)
	if session != "" {
		fmt.Fprintf(&b, "Session: %s\r\n", session)
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
		return nil, fmt.Errorf("raop2: 空响应行")
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("raop2: 无法解析状态行 %q", line)
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("raop2: 状态码非法 %q", parts[1])
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

func randomUint64() uint64 {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint64(time.Now().UnixNano())
	}
	return uint64(b[0])<<56 | uint64(b[1])<<48 | uint64(b[2])<<40 | uint64(b[3])<<32 |
		uint64(b[4])<<24 | uint64(b[5])<<16 | uint64(b[6])<<8 | uint64(b[7])
}
