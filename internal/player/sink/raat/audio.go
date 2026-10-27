package raat

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"net"
	"sync"
	"time"
)

// 音频通道命令前缀（规格 §④）。
const (
	cmdData          = 0 // PCM 帧（裸字节）
	cmdChmap         = 1 // 声道映射（每声道 4B 大端 ID）
	cmdStreamID      = 2 // [4B 大端 stream_id] — 流代际切换
	cmdStreamSample  = 3 // [8B 大端 stream_sample] — 本包起始采样位置
	cmdNormalization = 4 // [8B gain][8B peak]（IEEE754 大端）
	cmdCodecData     = 5 // FLAC 编码帧
)

// AudioConn RAAT 音频数据通道（TCP，1 字节命令前缀协议）。
//
// 写状态机（对齐 RaatTcpAudioSource）：stream_id 变化先发 STREAMID；
// 采样不连续（sample != last+len）先发 STREAMSAMPLE；每帧 PCM 一个 DATA
// 命令（分发帧 882 采样，TCP 自分段）。
type AudioConn struct {
	conn net.Conn
	mu   sync.Mutex

	streamID   int32
	haveID     bool
	lastSample int64
	haveSample bool
	lastLen    int // 上一包采样数
}

// DialAudio 连接音频数据通道。
func DialAudio(ctx context.Context, addr string) (*AudioConn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("raat/audio: 连接 %s 失败: %w", addr, err)
	}
	return &AudioConn{conn: conn}, nil
}

// NewAudioConn 包装既有连接（测试用）。
func NewAudioConn(conn net.Conn) *AudioConn { return &AudioConn{conn: conn} }

// Close 关闭连接。
func (a *AudioConn) Close() error { return a.conn.Close() }

// SetStreamID 声明流代际：下一次写数据前先发 [2][4B BE id]。
func (a *AudioConn) SetStreamID(id int32) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.streamID = id
	// 立即发出（对齐"ID 变化先发"）；也可延迟到下一帧——这里选择立即，
	// 与 RaatTcpAudioSource 的 _stream_id 检查点（每帧写前）等价。
	if err := a.writeCmdLocked(cmdStreamID, beInt32(id)); err != nil {
		return err
	}
	a.haveID = true
	// 新流代际：采样连续性重置
	a.haveSample = false
	return nil
}

// WritePCM 写一帧 PCM（裸字节）。sample 为该帧在流内的起始采样位置；
// 与上一帧不连续时先发 [3][8B BE sample]。
func (a *AudioConn) WritePCM(sample int64, pcm []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.writePCMLocked(sample, pcm, cmdData)
}

// WriteCodec 写一帧编码数据（FLAC，COMMAND_CODEC_DATA=5）。
func (a *AudioConn) WriteCodec(sample int64, frame []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.writePCMLocked(sample, frame, cmdCodecData)
}

func (a *AudioConn) writePCMLocked(sample int64, data []byte, dataCmd byte) error {
	// stream_id 未声明先发（首写兜底；正常路径 SetStreamID 已发）
	if !a.haveID {
		if err := a.writeCmdLocked(cmdStreamID, beInt32(a.streamID)); err != nil {
			return err
		}
		a.haveID = true
	}
	// 采样不连续（含首包）先发 STREAMSAMPLE
	if !a.haveSample || sample != a.lastSample+int64(a.lastLen) {
		if err := a.writeCmdLocked(cmdStreamSample, beInt64(sample)); err != nil {
			return err
		}
	}
	// 数据帧：[1B 命令][载荷]
	if err := a.writeCmdLocked(dataCmd, data); err != nil {
		return err
	}
	a.lastSample = sample
	a.lastLen = len(data) / 4 // s16le 立体声一采样 4 字节（构造方保证）
	a.haveSample = true
	return nil
}

// WriteNormalization 音量平整元数据：[4][8B gain][8B peak]（IEEE754 大端）。
func (a *AudioConn) WriteNormalization(gain, peak float64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	payload := make([]byte, 16)
	binary.BigEndian.PutUint64(payload[0:8], float64bits(gain))
	binary.BigEndian.PutUint64(payload[8:16], float64bits(peak))
	return a.writeCmdLocked(cmdNormalization, payload)
}

func (a *AudioConn) writeCmdLocked(cmd byte, payload []byte) error {
	buf := make([]byte, 1+len(payload))
	buf[0] = cmd
	copy(buf[1:], payload)
	_, err := a.conn.Write(buf)
	return err
}

func beInt32(v int32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], uint32(v))
	return b[:]
}

func beInt64(v int64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	return b[:]
}

func float64bits(f float64) uint64 { return math.Float64bits(f) }
