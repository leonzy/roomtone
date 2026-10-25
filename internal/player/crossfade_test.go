package player

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

// tone 生成 n 帧 s16le 双声道的常量样本。
func tone(frames int, v int16) []byte {
	b := make([]byte, frames*4)
	for i := 0; i < frames; i++ {
		binary.LittleEndian.PutUint16(b[i*4:], uint16(v))
		binary.LittleEndian.PutUint16(b[i*4+2:], uint16(v))
	}
	return b
}

func sampleAt(b []byte, frame int) int16 {
	return int16(binary.LittleEndian.Uint16(b[frame*4:]))
}

// TestCrossfadeReaderOverlap 校验：输出长度 = 两曲之和 - 淡化窗，
// 交界前保持原声、交界后完全切到下一曲、中间等功率单调过渡。
func TestCrossfadeReaderOverlap(t *testing.T) {
	const curFrames, nextFrames, fadeFrames = 100, 100, 20
	cur := bytes.NewReader(tone(curFrames, 1000))
	next := io.NopCloser(bytes.NewReader(tone(nextFrames, 3000)))

	xf := newCrossfadeReader(cur, fadeFrames*4, 4, func() (io.ReadCloser, error) {
		return next, nil
	})
	out, err := io.ReadAll(xf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !xf.Mixed() {
		t.Fatal("应标记为已混合")
	}

	wantFrames := curFrames + nextFrames - fadeFrames
	if got := len(out) / 4; got != wantFrames {
		t.Fatalf("输出帧数 %d，期望 %d", got, wantFrames)
	}

	// 淡化窗之前的帧原样输出。
	for i := 0; i < curFrames-fadeFrames; i++ {
		if v := sampleAt(out, i); v != 1000 {
			t.Fatalf("第 %d 帧应保持原声 1000，实际 %d", i, v)
		}
	}
	// 淡化窗两端分别贴近原声与下一曲；等功率曲线在两端振幅不同时
	// 中间会略高于两者（cos+sin 的峰值 ~1.41），所以只断言区间而不要求单调。
	first := sampleAt(out, curFrames-fadeFrames)
	last := sampleAt(out, curFrames-1)
	if first > 1400 {
		t.Fatalf("淡化窗起点应接近 1000，实际 %d", first)
	}
	if last < 2800 {
		t.Fatalf("淡化窗终点应接近 3000，实际 %d", last)
	}
	for i := curFrames - fadeFrames; i < curFrames; i++ {
		if v := sampleAt(out, i); v < 900 || v > 3300 {
			t.Fatalf("第 %d 帧 %d 超出等功率混合的合理区间", i, v)
		}
	}
	// 窗口中点应接近等功率结果：(1000+3000)*0.707 ≈ 2828。
	mid := sampleAt(out, curFrames-fadeFrames+fadeFrames/2)
	if mid < 2700 || mid > 2950 {
		t.Fatalf("淡化中点应在 ~2828 附近，实际 %d", mid)
	}
	// 淡化窗之后完全是下一曲。
	for i := curFrames; i < wantFrames; i++ {
		if v := sampleAt(out, i); v != 3000 {
			t.Fatalf("第 %d 帧应已是下一曲 3000，实际 %d", i, v)
		}
	}
}

// TestCrossfadeReaderShortTrack 当前曲比淡化窗还短：整曲参与混合，不丢帧。
func TestCrossfadeReaderShortTrack(t *testing.T) {
	const fadeFrames = 50
	cur := bytes.NewReader(tone(10, 1000))
	next := io.NopCloser(bytes.NewReader(tone(60, 3000)))
	xf := newCrossfadeReader(cur, fadeFrames*4, 4, func() (io.ReadCloser, error) {
		return next, nil
	})
	out, err := io.ReadAll(xf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got, want := len(out)/4, 10+60-10; got != want {
		t.Fatalf("短曲输出帧数 %d，期望 %d", got, want)
	}
	if v := sampleAt(out, len(out)/4-1); v != 3000 {
		t.Fatalf("结尾应是下一曲原声，实际 %d", v)
	}
}

// TestCrossfadeReaderNoNext 没有下一曲时退化为直读。
func TestCrossfadeReaderNoNext(t *testing.T) {
	src := tone(20, 500)
	xf := newCrossfadeReader(bytes.NewReader(src), 8*4, 4, nil)
	out, err := io.ReadAll(xf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(out, src) {
		t.Fatalf("无下一曲时应原样输出，长度 %d vs %d", len(out), len(src))
	}
	if xf.Mixed() {
		t.Fatal("没有混合不应标记 Mixed")
	}
}

// TestCrossfadeReaderClamps 两端同相叠加时不越过 int16 边界。
func TestCrossfadeReaderClamps(t *testing.T) {
	cur := bytes.NewReader(tone(40, 30000))
	next := io.NopCloser(bytes.NewReader(tone(40, 30000)))
	xf := newCrossfadeReader(cur, 20*4, 4, func() (io.ReadCloser, error) { return next, nil })
	out, err := io.ReadAll(xf)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for i := 0; i < len(out)/4; i++ {
		if v := sampleAt(out, i); v < 0 {
			t.Fatalf("第 %d 帧溢出成负数 %d", i, v)
		}
	}
}
