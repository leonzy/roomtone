package raat

import (
	"bufio"
	"context"
	"encoding/binary"
	"math"
	"net"
	"testing"
	"time"
)

// pipeReader 同步读助手：每条 expect 恰好读一条命令帧。
// net.Pipe 的 Write 阻塞到对端读走数据——同步逐帧读与写侧天然握手，
// 无需后台协程（也避免变长 DATA 帧的长度竞态）。
type pipeReader struct {
	br *bufio.Reader
}

func newPipeReader(c net.Conn) *pipeReader {
	return &pipeReader{br: bufio.NewReader(c)}
}

// expect 读一条命令帧。cmd 为期望的命令前缀；dataLen 为 DATA/CODEC_DATA
// 的载荷长度（定长命令忽略该参数）。
func (r *pipeReader) expect(t *testing.T, cmd byte, dataLen int) []byte {
	t.Helper()
	head := make([]byte, 1)
	if _, err := readFull2(r.br, head); err != nil {
		t.Fatalf("读命令前缀: %v", err)
	}
	if head[0] != cmd {
		t.Fatalf("命令 = %d，期望 %d", head[0], cmd)
	}
	n := 0
	switch cmd {
	case cmdStreamID:
		n = 4
	case cmdStreamSample:
		n = 8
	case cmdNormalization:
		n = 16
	default:
		n = dataLen
	}
	payload := make([]byte, n)
	if n > 0 {
		if _, err := readFull2(r.br, payload); err != nil {
			t.Fatalf("读载荷（cmd=%d）: %v", cmd, err)
		}
	}
	return payload
}

func readFull2(br *bufio.Reader, buf []byte) (int, error) {
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

func TestAudioConnStateMachine(t *testing.T) {
	// 用真实 TCP 而非 net.Pipe：Pipe 的 Write 阻塞到对端读走，
	// 写后读的顺序测试会死锁；TCP 内核缓冲让同步逐帧读无竞态。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srvCh := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		srvCh <- conn
	}()
	a, err := DialAudio(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var c2 net.Conn
	select {
	case c2 = <-srvCh:
	case <-time.After(2 * time.Second):
		t.Fatal("设备侧未接入")
	}
	defer c2.Close()
	r := newPipeReader(c2)

	// SetStreamID(7) → [1B=2][4B BE 7]
	if err := a.SetStreamID(7); err != nil {
		t.Fatal(err)
	}
	got := r.expect(t, cmdStreamID, 0)
	if binary.BigEndian.Uint32(got) != 7 {
		t.Fatalf("STREAMID = %d，期望 7", binary.BigEndian.Uint32(got))
	}

	// WritePCM(0, pcm) 首写 → [3][8B BE 0]（首包必发）紧跟 [0]+pcm
	pcm := make([]byte, 882*4)
	for i := range pcm {
		pcm[i] = byte(i % 7)
	}
	if err := a.WritePCM(0, pcm); err != nil {
		t.Fatal(err)
	}
	got = r.expect(t, cmdStreamSample, 0)
	if binary.BigEndian.Uint64(got) != 0 {
		t.Fatalf("首包 STREAMSAMPLE = %d，期望 0", binary.BigEndian.Uint64(got))
	}
	data := r.expect(t, cmdData, len(pcm))
	for i := range pcm {
		if data[i] != pcm[i] {
			t.Fatalf("DATA 第 %d 字节不符", i)
		}
	}

	// 连续写 WritePCM(441, pcm2)（= 0+882... 注意 lastLen=882 采样）→ 只读 DATA
	pcm2 := make([]byte, 441*4)
	if err := a.WritePCM(882, pcm2); err != nil {
		t.Fatal(err)
	}
	r.expect(t, cmdData, len(pcm2))

	// 跳变 WritePCM(90000, pcm3) → 先 STREAMSAMPLE(90000) 再 DATA
	pcm3 := make([]byte, 882*4)
	if err := a.WritePCM(90000, pcm3); err != nil {
		t.Fatal(err)
	}
	got = r.expect(t, cmdStreamSample, 0)
	if binary.BigEndian.Uint64(got) != 90000 {
		t.Fatalf("跳变 STREAMSAMPLE = %d，期望 90000", binary.BigEndian.Uint64(got))
	}
	r.expect(t, cmdData, len(pcm3))

	// SetStreamID(8) 后再写 → 又出现 STREAMID 帧（ID 变化先发）
	if err := a.SetStreamID(8); err != nil {
		t.Fatal(err)
	}
	got = r.expect(t, cmdStreamID, 0)
	if binary.BigEndian.Uint32(got) != 8 {
		t.Fatalf("新 STREAMID = %d，期望 8", binary.BigEndian.Uint32(got))
	}
	// 新流代际后首写：采样连续性重置 → 再发 STREAMSAMPLE
	if err := a.WritePCM(0, pcm); err != nil {
		t.Fatal(err)
	}
	r.expect(t, cmdStreamSample, 0)
	r.expect(t, cmdData, len(pcm))

	// WriteNormalization → [4][8B gain][8B peak]（IEEE754 大端）
	if err := a.WriteNormalization(-3.5, 0.99); err != nil {
		t.Fatal(err)
	}
	got = r.expect(t, cmdNormalization, 0)
	if math.Float64frombits(binary.BigEndian.Uint64(got[0:8])) != -3.5 {
		t.Fatalf("gain 不符: %v", math.Float64frombits(binary.BigEndian.Uint64(got[0:8])))
	}
	if math.Float64frombits(binary.BigEndian.Uint64(got[8:16])) != 0.99 {
		t.Fatal("peak 不符")
	}

	// WriteCodec → [5]+帧（B9 用）
	if err := a.WriteCodec(0, []byte{0xAA, 0xBB}); err != nil {
		t.Fatal(err)
	}
	r.expect(t, cmdStreamSample, 0) // 上一 DATA 后连续性重算
	r.expect(t, cmdCodecData, 2)
}

// DialAudio 连接真实 TCP listener。
func TestDialAudio(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			conn.Close()
		}
	}()
	a, err := DialAudio(context.Background(), ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
}
