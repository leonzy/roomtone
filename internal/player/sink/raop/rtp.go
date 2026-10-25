package raop

import (
	"encoding/binary"
	"time"
)

const (
	// PayloadType RTP 载荷类型 96（动态），RAOP 固定用它。
	PayloadType byte = 0x60
	// ControlSync 同步包类型（RTP 扩展 0x54 + marker 位）。
	ControlSync byte = 0xd4
	// ControlTiming 定时包类型（0x56 + marker 位）。
	ControlTiming byte = 0xd6
	// rtpHeaderLen RTP 固定头长度。
	rtpHeaderLen = 12
	// NTPEpochOffset 1900-01-01 到 1970-01-01 的秒数。
	NTPEpochOffset = 2208988800
)

// NTPTime NTP 时间戳（64 位定点）。
type NTPTime struct {
	Sec  uint32
	Frac uint32
}

// ToNTP 把墙上时间转成 NTP。
func ToNTP(t time.Time) NTPTime {
	sec := uint32(t.Unix() + NTPEpochOffset)
	frac := uint32((uint64(t.Nanosecond()) << 32) / 1e9)
	return NTPTime{Sec: sec, Frac: frac}
}

// BuildRTPPacket 拼一个音频包：12 字节头 + PCM 载荷。
//
// marker 位在每首歌第一包上置 1，设备据此重置抖动缓冲。
func BuildRTPPacket(seq uint16, timestamp, ssrc uint32, marker bool, payload []byte) []byte {
	out := make([]byte, rtpHeaderLen+len(payload))
	out[0] = 0x80
	out[1] = PayloadType
	if marker {
		out[1] |= 0x80
	}
	binary.BigEndian.PutUint16(out[2:4], seq)
	binary.BigEndian.PutUint32(out[4:8], timestamp)
	binary.BigEndian.PutUint32(out[8:12], ssrc)
	copy(out[rtpHeaderLen:], payload)
	return out
}

// BuildSyncPacket 拼同步包：告诉设备"RTP 时间戳 X 对应 NTP 时间 Y"。
//
// rtpNow 是当前已发出的最新时间戳，rtpAtPlay 是应开始发声的那个时间戳。
// 设备用两者之差算出自己还要缓冲多久。
func BuildSyncPacket(rtpNow, rtpAtPlay uint32, ntp NTPTime) []byte {
	out := make([]byte, 20)
	out[0] = 0x80
	out[1] = ControlSync
	binary.BigEndian.PutUint16(out[2:4], 7)
	binary.BigEndian.PutUint32(out[4:8], rtpNow)
	binary.BigEndian.PutUint32(out[8:12], ntp.Sec)
	binary.BigEndian.PutUint32(out[12:16], ntp.Frac)
	binary.BigEndian.PutUint32(out[16:20], rtpAtPlay)
	return out
}

// BuildTimingPacket 拼定时包（NTP 往返测量）。
func BuildTimingPacket(origin, receive, transmit NTPTime) []byte {
	out := make([]byte, 32)
	out[0] = 0x80
	out[1] = ControlTiming
	binary.BigEndian.PutUint16(out[2:4], 7)
	binary.BigEndian.PutUint32(out[4:8], origin.Sec)
	binary.BigEndian.PutUint32(out[8:12], origin.Frac)
	binary.BigEndian.PutUint32(out[12:16], receive.Sec)
	binary.BigEndian.PutUint32(out[16:20], receive.Frac)
	binary.BigEndian.PutUint32(out[20:24], transmit.Sec)
	binary.BigEndian.PutUint32(out[24:28], transmit.Frac)
	return out
}

// Packetizer 维护 seq 与时间戳状态，供 sink 逐帧调用。
type Packetizer struct {
	SSRC uint32
	Seq  uint16
}

// Next 取下一个包。samples 是这一帧的采样数，时间戳随之推进。
func (p *Packetizer) Next(timestamp uint32, samples int, payload []byte, marker bool) []byte {
	pkt := BuildRTPPacket(p.Seq, timestamp, p.SSRC, marker, payload)
	p.Seq++
	return pkt
}
