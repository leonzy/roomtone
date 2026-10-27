package raop

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// SDP 一次 ANNOUNCE 需要的信息。
type SDP struct {
	ALAC bool
	// Origin 发起端 IP，设备用它判断是否同一网段。
	Origin      string
	Destination string
	// SessionID 任意 32 位数字，同一会话内保持不变。
	SessionID  uint32
	SampleRate int
	Channels   int
	// FrameSamples 一包里的采样数（352 = 8ms @44.1k）。
	FrameSamples int
	// AESKey / AESIV 仅在加密模式填写；为空则声明明文 L16。
	AESKey string
	AESIV  string
}

// BuildSDP 生成 ANNOUNCE 的 body。
//
// 设备对字段顺序很敏感（尤其 a=rtpmap 必须在 a=ptime 之前），所以这里
// 用固定顺序拼字符串而不是 map。
func BuildSDP(s SDP) []byte {
	if s.SampleRate == 0 {
		s.SampleRate = 44100
	}
	if s.Channels == 0 {
		s.Channels = 2
	}
	if s.FrameSamples == 0 {
		s.FrameSamples = 352
	}
	if s.Origin == "" {
		s.Origin = "127.0.0.1"
	}
	var b strings.Builder
	b.WriteString("v=0\r\n")
	fmt.Fprintf(&b, "o=iTunes %d 0 IN IP4 %s\r\n", s.SessionID, s.Origin)
	b.WriteString("s=iTunes\r\n")
	destination := s.Destination
	if destination == "" {
		destination = s.Origin
	}
	fmt.Fprintf(&b, "c=IN IP4 %s\r\n", destination)
	b.WriteString("t=0 0\r\n")
	b.WriteString("m=audio 0 RTP/AVP 96\r\n")
	if s.ALAC {
		b.WriteString("a=rtpmap:96 AppleLossless\r\n")
		fmt.Fprintf(&b, "a=fmtp:96 %d 0 16 40 10 14 %d 255 0 0 %d\r\n", s.FrameSamples, s.Channels, s.SampleRate)
	} else {
		fmt.Fprintf(&b, "a=rtpmap:96 L16/%d/%d\r\n", s.SampleRate, s.Channels)
	}
	if !s.ALAC {
		fmt.Fprintf(&b, "a=ptime:%d\r\n", s.FrameSamples)
		fmt.Fprintf(&b, "a=maxptime:%d\r\n", s.FrameSamples)
		fmt.Fprintf(&b, "a=minptime:%d\r\n", s.FrameSamples)
	}
	if s.AESKey != "" {
		fmt.Fprintf(&b, "a=rsaaeskey:%s\r\n", s.AESKey)
	}
	if s.AESIV != "" {
		fmt.Fprintf(&b, "a=aesiv:%s\r\n", s.AESIV)
	}
	return []byte(b.String())
}

// EncodeIV base64 编码 IV，空 IV 返回空串。
func EncodeIV(iv []byte) string {
	if len(iv) == 0 {
		return ""
	}
	return base64.RawStdEncoding.EncodeToString(iv)
}
