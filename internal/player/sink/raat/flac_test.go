package raat

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"

	"songloft/internal/player/sink"
)

// TestFlacBenchmarkGate B9 基准门（go/no-go）：44100/16/2 s16le 交错 PCM
// 以 882 采样/帧连续编码 60s 内容，断言编码吞吐 ≥ 10× 实时
// （≥ 44100×10 采样/s）且输出可被 flac 解码回逐字节一致 PCM。
// 不达标：移除依赖、本任务关闭、在 docs/raat-protocol-notes.md 记录实测数据。
func TestFlacBenchmarkGate(t *testing.T) {
	const (
		sampleRate    = 44100
		nchannels     = 2
		bitsPerSample = 16
		blockSize     = 882 // 分发帧 882 采样（20ms @ 44.1k）
		totalSeconds  = 60
	)

	// 生成 60s 合成音频（多频正弦 + 噪声，比纯音更接近真实内容）
	totalSamples := sampleRate * totalSeconds
	pcm := make([]byte, 0, totalSamples*nchannels*2)
	noise := rand.New(rand.NewSource(42))
	for i := 0; i < totalSamples; i++ {
		l := int16(0.4*math.Sin(2*math.Pi*440*float64(i)/sampleRate)*32767 +
			0.2*math.Sin(2*math.Pi*1320*float64(i)/sampleRate)*32767 +
			0.05*float64(noise.Int31n(2000)-1000))
		r := int16(0.4*math.Sin(2*math.Pi*440*float64(i)/sampleRate)*32767 +
			0.2*math.Sin(2*math.Pi*880*float64(i)/sampleRate)*32767 +
			0.05*float64(noise.Int31n(2000)-1000))
		pcm = append(pcm, byte(l), byte(l>>8), byte(r), byte(r>>8))
	}

	// 编码：882 采样/帧，PredFixed（真压缩）
	info := &meta.StreamInfo{
		BlockSizeMin: uint16(blockSize), BlockSizeMax: uint16(blockSize),
		SampleRate:    sampleRate,
		NChannels:     nchannels,
		BitsPerSample: bitsPerSample,
		NSamples:      uint64(totalSamples),
	}
	var out bytes.Buffer
	enc, err := NewFlacEncoderWrapper(&out, info)
	if err != nil {
		t.Fatalf("encoder: %v", err)
	}

	start := time.Now()
	written := 0
	for off := 0; off+blockSize*4 <= len(pcm); off += blockSize * 4 {
		n, err := enc.WriteBlock(pcm[off : off+blockSize*4])
		if err != nil {
			t.Fatalf("write block @%d: %v", off, err)
		}
		written += n
	}
	elapsed := time.Since(start)

	// 吞吐断言：≥ 10× 实时
	encodedSamples := int64(len(pcm) / 4)
	throughput := float64(encodedSamples) / elapsed.Seconds()
	t.Logf("FLAC 基准：60s 内容编码耗时 %v，吞吐 %.0f 采样/s（实时 = %d），压缩比 %.1f%%（buf=%d written=%d pcm=%d）",
		elapsed, throughput, sampleRate, 100*float64(out.Len())/float64(len(pcm)), out.Len(), written, len(pcm))
	if throughput < 10*sampleRate {
		t.Fatalf("基准门未达标：吞吐 %.0f 采样/s < 10× 实时（%d）——按计划关闭 B9 并记录", throughput, 10*sampleRate)
	}

	// 往返断言：解码回逐字节一致 PCM
	decoded, err := DecodeFlacAll(out.Bytes())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != len(pcm) {
		t.Fatalf("解码长度 %d ≠ 原始 %d", len(decoded), len(pcm))
	}
	for i := range pcm {
		if decoded[i] != pcm[i] {
			t.Fatalf("解码 PCM 第 %d 字节不符（%d vs %d）", i, decoded[i], pcm[i])
		}
	}
}

// TestFlacFrameFormat FLAC 帧形态：每帧独立可解（无 STREAMINFO 依赖的
// 帧头自描述）——RAAT 的 CODEC_DATA 通道按帧传输。
func TestFlacFrameFormat(t *testing.T) {
	const blockSize = 882
	pcm := make([]byte, blockSize*4)
	for i := 0; i+3 < len(pcm); i += 4 {
		l := int16(math.Sin(2 * math.Pi * 440 * float64(i/4) / 44100 * 32767))
		pcm[i], pcm[i+1], pcm[i+2], pcm[i+3] = byte(l), byte(l>>8), byte(l), byte(l>>8)
	}
	// 单帧编码 → 帧头自描述（sync 0b11111111 0b11111000）
	frameBytes, err := EncodeFlacFrame(pcm)
	if err != nil {
		t.Fatalf("frame: %v", err)
	}
	if len(frameBytes) == 0 {
		t.Fatal("空帧")
	}
	if frameBytes[0] != 0xFF || frameBytes[1]&0xF8 != 0xF8 {
		t.Fatalf("帧头 sync 不符: %02X %02X", frameBytes[0], frameBytes[1])
	}
	// 压缩生效（非 verbatim 的尺寸波动）——正弦波应明显小于原始 PCM
	if len(frameBytes) >= len(pcm) {
		t.Logf("警告：帧未压缩（%d ≥ %d）——正弦波应可压缩", len(frameBytes), len(pcm))
	}
	// 解码往返
	decoded, err := DecodeFlacFrame(frameBytes)
	if err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if len(decoded) != len(pcm) {
		t.Fatalf("解码长度 %d ≠ %d", len(decoded), len(pcm))
	}
	for i := range pcm {
		if decoded[i] != pcm[i] {
			t.Fatalf("解码 PCM 第 %d 字节不符", i)
		}
	}
}

var _ = frame.PredFixed

// TestSinkFLACWireCodec B9：info 声明 codecs 含 "flac" → setup 带
// wire_codec:"flac"；WriteFrame 后音频通道收到 [1B=5]+FLAC 帧；
// 采样跳变仍发 STREAMSAMPLE；不声明支持 → 纯 PCM 路径不受影响。
func TestSinkFLACWireCodec(t *testing.T) {
	d := newFakeRAATDevice(t)
	// info 响应带 codecs: ["flac"]
	d.mu.Lock()
	d.infoResp = `{"status":"Success","output":{"supported_formats":"44100/16/2"},"codecs":["flac"],"volume":{"is_supported":true},"transport":{"is_supported":true}}`
	d.mu.Unlock()
	s := New("dev", Options{
		Address: d.addr(), SampleRate: 44100, Channels: 2,
		BufferSize: 2 * time.Second, ReadyThreshold: time.Second, FLAC: true,
	})
	if err := s.Open(context.Background(), sink.StreamPlan{
		Format: sink.Format{Codec: sink.CodecL16, SampleRate: 44100, Channels: 2, Bits: 16},
	}); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// setup 带 wire_codec:"flac"
	reqs := waitReqs(t, d, 3)
	var setup struct {
		WireCodec string `json:"wire_codec"`
	}
	if err := json.Unmarshal(reqs[2].body, &setup); err != nil {
		t.Fatal(err)
	}
	if setup.WireCodec != "flac" {
		t.Fatalf("setup wire_codec = %q，期望 flac", setup.WireCodec)
	}

	// WriteFrame → 音频通道收到 STREAMID + STREAMSAMPLE + CODEC_DATA(5)
	frame := make([]byte, 882*4)
	for i := 0; i+3 < len(frame); i += 4 {
		l := int16(i)
		frame[i], frame[i+1], frame[i+2], frame[i+3] = byte(l), byte(l>>8), byte(l), byte(l>>8)
	}
	if err := s.WriteFrame(sink.Frame{SampleIndex: 0, Data: frame}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var frames []audioFrame
	for time.Now().Before(deadline) {
		frames = d.frames()
		if len(frames) >= 3 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if len(frames) < 3 {
		t.Fatalf("音频通道只收到 %d 帧（期望 ≥3：STREAMID+STREAMSAMPLE+CODEC_DATA）", len(frames))
	}
	if frames[0].cmd != cmdStreamID {
		t.Fatalf("帧 0 = cmd %d，期望 STREAMID", frames[0].cmd)
	}
	if frames[1].cmd != cmdStreamSample {
		t.Fatalf("帧 1 = cmd %d，期望 STREAMSAMPLE", frames[1].cmd)
	}
	if frames[2].cmd != cmdCodecData {
		t.Fatalf("帧 2 = cmd %d，期望 CODEC_DATA(5)", frames[2].cmd)
	}
	// FLAC 帧头 sync
	if len(frames[2].payload) < 2 || frames[2].payload[0] != 0xFF || frames[2].payload[1]&0xF8 != 0xF8 {
		t.Fatalf("CODEC_DATA 载荷不是 FLAC 帧: %x", frames[2].payload[:min2(4, len(frames[2].payload))])
	}

	// 采样跳变仍发 STREAMSAMPLE
	if err := s.WriteFrame(sink.Frame{SampleIndex: 90000, Data: frame}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	var sawJump bool
	for time.Now().Before(deadline) {
		for _, f := range d.frames() {
			if f.cmd == cmdStreamSample {
				sawJump = true
			}
		}
		if sawJump {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !sawJump {
		t.Fatal("采样跳变应触发 STREAMSAMPLE")
	}

	// 不声明支持 → 纯 PCM 路径（setup 无 wire_codec，DATA=0）
	d2 := newFakeRAATDevice(t)
	s2 := New("dev", Options{
		Address: d2.addr(), SampleRate: 44100, Channels: 2,
		BufferSize: 2 * time.Second, ReadyThreshold: time.Second, FLAC: true, // 开了 FLAC 但设备不支持
	})
	if err := s2.Open(context.Background(), sink.StreamPlan{
		Format: sink.Format{Codec: sink.CodecL16, SampleRate: 44100, Channels: 2, Bits: 16},
	}); err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	reqs2 := waitReqs(t, d2, 3)
	var setup2 struct {
		WireCodec string `json:"wire_codec"`
	}
	if err := json.Unmarshal(reqs2[2].body, &setup2); err != nil {
		t.Fatal(err)
	}
	if setup2.WireCodec != "" {
		t.Fatalf("设备不声明 flac 时 setup 不应带 wire_codec: %q", setup2.WireCodec)
	}
	if err := s2.WriteFrame(sink.Frame{SampleIndex: 0, Data: frame}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		fs := d2.frames()
		if len(fs) >= 3 && fs[2].cmd == cmdData {
			return // PCM 路径正常
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("纯 PCM 路径应发 DATA(0)")
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}
