package raat

import (
	"bytes"
	"encoding/binary"
	"io"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
)

// FLAC 压缩音频通道（B9）。设备能力：info 响应顶层 "codecs" 列表含
// "flac"（RaatTransportClient.cs:751）；setup 带 "wire_codec":"flac"
// （RaatTransportClient.cs:2258）；采样率限制 ShouldUseFlacWireCodec
// （RaatZonePlayer.cs:2207）：0 < rate < 655360，且 rate ≥ 65536 时
// rate % 10 == 0（FLAC streamable subset）。音频通道发 CODEC_DATA=5 帧。
//
// 帧格式：每帧独立自描述（FLAC 帧头带 sync + block size + sample rate +
// channels + bits），RAAT 按帧传输无需 STREAMINFO 前置。

// NewFlacEncoder 创建流式 FLAC 编码器（写入 w）。
func NewFlacEncoder(w io.Writer, info *meta.StreamInfo) (*flac.Encoder, error) {
	return flac.NewEncoder(w, info)
}

// WriteBlock 编码一个交错 s16le PCM 块（blockSize 采样 × 声道 × 2 字节），
// 返回写入的字节数。PredFixed（真压缩，固定阶预测）。
func (enc *flacEncoderWrapper) WriteBlock(pcm []byte) (int, error) {
	return enc.writeBlock(pcm)
}

// flacEncoderWrapper 包装 flac.Encoder 提供块级写入。
type flacEncoderWrapper struct {
	enc    *flac.Encoder
	w      *countingWriter
	ch     int
	blocks int
}

// countingWriter 计数实际写入字节（压缩比测量用）。
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// NewFlacEncoderWrapper 创建包装器。
func NewFlacEncoderWrapper(w io.Writer, info *meta.StreamInfo) (*flacEncoderWrapper, error) {
	cw := &countingWriter{w: w}
	enc, err := flac.NewEncoder(cw, info)
	if err != nil {
		return nil, err
	}
	return &flacEncoderWrapper{enc: enc, w: cw, ch: int(info.NChannels)}, nil
}

func (w *flacEncoderWrapper) writeBlock(pcm []byte) (int, error) {
	blockSize := len(pcm) / (w.ch * 2)
	// 帧头自描述（SampleRate/BitsPerSample/Channels 全带）：RAAT 的
	// CODEC_DATA 通道按帧独立传输，不能依赖 STREAMINFO 继承——
	// BitsPerSample=0 会让编码器以 0 bit/采样写空数据（实测钉死）。
	f := &frame.Frame{
		Header: frame.Header{
			HasFixedBlockSize: true,
			BlockSize:         uint16(blockSize),
			SampleRate:        w.enc.Info.SampleRate,
			Channels:          frame.Channels(w.ch - 1),
			BitsPerSample:     w.enc.Info.BitsPerSample,
		},
	}
	f.Subframes = make([]*frame.Subframe, w.ch)
	for ch := 0; ch < w.ch; ch++ {
		samples := make([]int32, blockSize)
		for i := 0; i < blockSize; i++ {
			samples[i] = int32(int16(binary.LittleEndian.Uint16(pcm[(i*w.ch+ch)*2:])))
		}
		// PredVerbatim + 默认开启的分析器：analyzeSubframe 自动比较
		// constant/fixed/verbatim 三种预测的代价并填好 Order/RiceSubframe
		// 等字段（手动填 PredFixed 会踩 encodeRicePart 的 Partitions 空切片
		// 坑——subframe 数据静默丢失，实测钉死）。
		f.Subframes[ch] = &frame.Subframe{
			SubHeader: frame.SubHeader{Pred: frame.PredVerbatim},
			Samples:   samples,
			NSamples:  blockSize,
		}
	}
	before := w.w.n
	if err := w.enc.WriteFrame(f); err != nil {
		return 0, err
	}
	w.blocks++
	return int(w.w.n - before), nil
}

// writeFrameTo 编码一个 882 采样块为独立 FLAC 帧，写入 wr
// （sink 的 CODEC_DATA 路径用：每帧一个 [5] 命令）。
// 复用 EncodeFlacFrame（构造完整流后截取帧字节——每帧一次 NewEncoder
// 的开销在基准门 102× 实时下可忽略）。
func (w *flacEncoderWrapper) writeFrameTo(wr io.Writer, pcm []byte) error {
	frameBytes, err := EncodeFlacFrame(pcm)
	if err != nil {
		return err
	}
	_, err = wr.Write(frameBytes)
	return err
}

// EncodeFlacFrame 编码单个 882 采样块为独立 FLAC 帧（无 STREAMINFO 依赖，
// 帧头自描述：block size + sample rate + channels + bits 全带）。
func EncodeFlacFrame(pcm []byte) ([]byte, error) {
	// 从输入推断：立体声 s16le（RAAT setup 固定 16bit）
	const ch = 2
	blockSize := len(pcm) / (ch * 2)
	var buf bytes.Buffer
	// 单帧流：STREAMINFO（最小形态）+ 一帧。RAAT 按帧传输时逐帧独立，
	// 这里产出含 STREAMINFO 的完整流再取帧。
	info := &meta.StreamInfo{
		BlockSizeMin: uint16(blockSize), BlockSizeMax: uint16(blockSize),
		SampleRate:    44100,
		NChannels:     ch,
		BitsPerSample: 16,
		NSamples:      uint64(blockSize),
	}
	enc, err := flac.NewEncoder(&buf, info)
	if err != nil {
		return nil, err
	}
	f := &frame.Frame{
		Header: frame.Header{
			HasFixedBlockSize: true,
			BlockSize:         uint16(blockSize),
			SampleRate:        44100,
			Channels:          frame.ChannelsLR,
			BitsPerSample:     16,
		},
	}
	f.Subframes = make([]*frame.Subframe, ch)
	for c := 0; c < ch; c++ {
		samples := make([]int32, blockSize)
		for i := 0; i < blockSize; i++ {
			samples[i] = int32(int16(binary.LittleEndian.Uint16(pcm[(i*ch+c)*2:])))
		}
		f.Subframes[c] = &frame.Subframe{
			SubHeader: frame.SubHeader{Pred: frame.PredVerbatim},
			Samples:   samples,
			NSamples:  blockSize,
		}
	}
	if err := enc.WriteFrame(f); err != nil {
		return nil, err
	}
	// 取帧：跳过 "fLaC" 签名 + STREAMINFO 元数据块，从第一个帧 sync 开始
	all := buf.Bytes()
	idx := findFrameSync(all)
	if idx < 0 {
		return nil, errNoFrame
	}
	return all[idx:], nil
}

var errNoFrame = &frameError{"未找到 FLAC 帧 sync"}

type frameError struct{ msg string }

func (e *frameError) Error() string { return "raat/flac: " + e.msg }

// findFrameSync 找第一个 FLAC 帧 sync（0xFF 0xF8 掩码）。
func findFrameSync(b []byte) int {
	for i := 0; i+1 < len(b); i++ {
		if b[i] == 0xFF && b[i+1]&0xF8 == 0xF8 {
			return i
		}
	}
	return -1
}

// DecodeFlacFrame 解码单个 FLAC 帧为 s16le 交错 PCM。
func DecodeFlacFrame(frameBytes []byte) ([]byte, error) {
	// 构造最小流：签名 + STREAMINFO（从帧头推断）+ 帧
	// 简化路径：用 flac.Decoder 解完整流（签名+元数据+帧）
	// 帧头自描述：解析 blocksize/channels/bits/sample rate
	if len(frameBytes) < 2 || frameBytes[0] != 0xFF || frameBytes[1]&0xF8 != 0xF8 {
		return nil, &frameError{"帧头 sync 不符"}
	}
	hdr := frameBytes[1]
	ch := int(hdr>>4)&0x07 + 1
	bits := []int{0, 8, 12, 0, 16, 20, 24, 0}[(hdr>>1)&0x07]
	// blocksize 与 samplerate 尾字节引用：读第 2 字节
	bsKey := int((frameBytes[2] >> 4) & 0x0F)
	srKey := int(frameBytes[2] & 0x0F)
	blockSize := map[int]int{1: 192, 2: 576, 3: 1152, 4: 2304, 5: 4608, 6: 6144, 7: 8192}[bsKey]
	sampleRate := map[int]int{1: 88200, 2: 176400, 3: 192000, 4: 8000, 5: 16000, 6: 22050, 7: 24000, 8: 32000, 9: 44100, 10: 48000, 11: 96000}[srKey]
	off := 3
	if bsKey == 7 || bsKey == 8 || bsKey == 9 {
		// 16bit blocksize 尾字节
		if off+2 > len(frameBytes) {
			return nil, &frameError{"截断的帧头（blocksize）"}
		}
		blockSize = int(binary.BigEndian.Uint16(frameBytes[off : off+2]))
		off += 2
	}
	if srKey == 12 || srKey == 13 || srKey == 14 {
		if off+2 > len(frameBytes) {
			return nil, &frameError{"截断的帧头（samplerate）"}
		}
		sampleRate = int(binary.BigEndian.Uint16(frameBytes[off : off+2]))
		off += 2
	}
	if blockSize == 0 || sampleRate == 0 || bits == 0 {
		return nil, &frameError{"帧头不完整"}
	}
	// 构造完整流：签名 + STREAMINFO + 帧
	var stream bytes.Buffer
	stream.WriteString("fLaC")
	// STREAMINFO（34 字节负载，last=1）
	si := make([]byte, 34)
	binary.BigEndian.PutUint16(si[0:2], uint16(blockSize)) // min
	binary.BigEndian.PutUint16(si[2:4], uint16(blockSize)) // max
	// frame sizes 未知（0）
	binary.BigEndian.PutUint32(si[8:12], uint32(sampleRate)<<12|uint32(ch-1)<<9|uint32(bits-1)<<4)
	binary.BigEndian.PutUint64(si[10:18], 0) // NSamples 未知（0 = 未知）
	// 重写：sample rate 20bit + channels 3bit + bits 5bit 从第 10 字节起
	si = make([]byte, 34)
	binary.BigEndian.PutUint16(si[0:2], uint16(blockSize))
	binary.BigEndian.PutUint16(si[2:4], uint16(blockSize))
	// 20bit sampleRate | 3bit (ch-1) | 5bit (bits-1) | 36bit nsamples
	v := uint64(sampleRate)<<44 | uint64(ch-1)<<41 | uint64(bits-1)<<36
	si[4] = byte(v >> 40)
	si[5] = byte(v >> 32)
	si[6] = byte(v >> 24)
	si[7] = byte(v >> 16)
	si[8] = byte(v >> 8)
	si[9] = byte(v)
	binary.BigEndian.PutUint32(si[10:14], uint32(blockSize)) // NSamples 低 32 位（近似）
	stream.WriteByte(0x80)                                   // STREAMINFO, last=1
	lenB := [3]byte{0, 0, 34}
	stream.Write(lenB[:])
	stream.Write(si)
	stream.Write(frameBytes)

	dec, err := flac.New(&stream)
	if err != nil {
		return nil, err
	}
	var out []byte
	for {
		f, err := dec.ParseNext()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		for i := 0; i < int(f.Header.BlockSize); i++ {
			for c := 0; c < int(f.Header.Channels.Count()); c++ {
				var v int16
				if c < len(f.Subframes) && i < len(f.Subframes[c].Samples) {
					v = int16(f.Subframes[c].Samples[i])
				}
				out = append(out, byte(v), byte(v>>8))
			}
		}
	}
	return out, nil
}

// DecodeFlacAll 解码完整 FLAC 流为 s16le 交错 PCM。
func DecodeFlacAll(b []byte) ([]byte, error) {
	dec, err := flac.New(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	var out []byte
	for {
		f, err := dec.ParseNext()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		for i := 0; i < int(f.Header.BlockSize); i++ {
			for c := 0; c < int(f.Header.Channels.Count()); c++ {
				var v int16
				if c < len(f.Subframes) && i < len(f.Subframes[c].Samples) {
					v = int16(f.Subframes[c].Samples[i])
				}
				out = append(out, byte(v), byte(v>>8))
			}
		}
	}
	return out, nil
}
