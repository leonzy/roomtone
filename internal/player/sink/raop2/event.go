package raop2

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"time"

	"songloft/internal/player/sink/airplay"
)

// connectEvent 连接 event 通道（TCP → eventPort），用 Events 密钥（交叉
// 派生）解密设备事件。
//
// 对齐 _EventBeginRecv：连接后常驻读循环；Roon 2.74 只解密记录不解析，
// 我们额外解析音量事件 → OnControl("setvolume")（计划 A2P-5 的扩展，
// 边界如实标注：事件载荷的完整语义无反编译依据，仅解析 bplist 的
// volume 键）。
func (s *Sink) connectEvent(ctx context.Context, eventPort int) error {
	addr := fmt.Sprintf("%s:%d", hostOf(s.opts.Address), eventPort)
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("raop2: event 通道连接失败: %w", err)
	}
	s.eventConn = conn
	// Events 密钥：交叉派生（写=Control-Read、读=Events-Write）。
	// 音频 ChaChaPoly 的 sessionKey 即 RTSP 加密的 sharedSecret。
	if s.sessionKey != nil {
		s.eventEnc = NewEncryptionContext(EncryptionChaChaPoly, ChannelEvents, s.pairing.SharedSecret())
	}
	go s.eventRecvLoop(conn)
	return nil
}

// eventRecvLoop 常驻读循环：解密（Events 读密钥）→ bplist 解析 →
// volume 键 → OnControl("setvolume", v/100)。
func (s *Sink) eventRecvLoop(conn net.Conn) {
	buf := make([]byte, 65536)
	var pending []byte
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if err != io.EOF {
				slog.Debug("raop2: event 通道关闭", "device", s.id, "err", err)
			}
			return
		}
		pending = append(pending, buf[:n]...)
		// 解密到块边界；不足一块留在 pending（残留缓冲）。
		var plain []byte
		if s.eventEnc != nil {
			pt, consumed, err := s.eventEnc.Decrypt(pending)
			if err != nil {
				slog.Debug("raop2: event 解密失败", "device", s.id, "err", err)
				pending = nil
				continue
			}
			plain = pt
			pending = append([]byte(nil), pending[consumed:]...)
		} else {
			plain = append([]byte(nil), pending...)
			pending = nil
		}
		if len(plain) == 0 {
			continue
		}
		s.handleEventPayload(plain)
	}
}

// handleEventPayload 解析事件载荷：bplist 的 volume 键 → OnControl。
func (s *Sink) handleEventPayload(plain []byte) {
	fields, err := airplay.DecodeBplistAny(plain)
	if err != nil {
		slog.Debug("raop2: event 载荷非 bplist", "device", s.id, "len", len(plain))
		return
	}
	if v, ok := plistNumber(fields, "volume"); ok && s.opts.OnControl != nil {
		// 设备音量事件（0-100）→ setvolume(0..1)。
		s.opts.OnControl("setvolume", v/100)
		return
	}
	slog.Debug("raop2: event 通道收到未解析事件", "device", s.id, "keys", keysOf(fields))
}

// plistNumber 取 plist 数字键（int64/uint64/float64 兼容）。
func plistNumber(m map[string]any, key string) (float64, bool) {
	switch v := m[key].(type) {
	case int64:
		return float64(v), true
	case uint64:
		return float64(v), true
	case int:
		return float64(v), true
	case float64:
		return v, true
	}
	return 0, false
}

func keysOf(m map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// readFileAll 小工具。
func readFileAll(path string) ([]byte, error) {
	return os.ReadFile(path)
}
