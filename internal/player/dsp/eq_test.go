package dsp

import (
	"encoding/binary"
	"math"
	"testing"
)

// tone 生成 s16le 双声道正弦（两声道相同）。
func tone(freq, rate float64, frames int, amp float64) []byte {
	buf := make([]byte, frames*4)
	for i := 0; i < frames; i++ {
		v := int16(amp * 32767 * math.Sin(2*math.Pi*freq*float64(i)/rate))
		binary.LittleEndian.PutUint16(buf[i*4:], uint16(v))
		binary.LittleEndian.PutUint16(buf[i*4+2:], uint16(v))
	}
	return buf
}

// goertzel 单频幅度估计，省掉一个 FFT 依赖。
func goertzel(samples []float64, freq, rate float64) float64 {
	w := 2 * math.Pi * freq / rate
	c := 2 * math.Cos(w)
	var s1, s2 float64
	for _, x := range samples {
		s0 := x + c*s1 - s2
		s2, s1 = s1, s0
	}
	return math.Sqrt(s1*s1+s2*s2-c*s1*s2) * 2 / float64(len(samples))
}

func leftChannel(pcm []byte) []float64 {
	out := make([]float64, len(pcm)/4)
	for i := range out {
		out[i] = float64(int16(binary.LittleEndian.Uint16(pcm[i*4:])))
	}
	return out
}

func TestFlatIsBitExact(t *testing.T) {
	const rate = 44100
	eq := NewEQ(rate, 2, make([]float64, BandCount), 0)
	if !eq.Flat() {
		t.Fatal("全 0 增益应为直通")
	}
	in := tone(1000, rate, 4410, 0.5)
	out := append([]byte(nil), in...)
	eq.ProcessInPlace(out)
	for i := range in {
		if in[i] != out[i] {
			t.Fatalf("直通不是逐字节相同，位置 %d", i)
		}
	}
}

func TestBoostAtCenterFreq(t *testing.T) {
	const rate = 44100
	bands := make([]float64, BandCount)
	bands[5] = 12 // 1000Hz +12dB
	eq := NewEQ(rate, 2, bands, 0)
	pcm := tone(1000, rate, rate/2, 0.05) // 500ms，幅度小以免削波
	before := goertzel(leftChannel(pcm), 1000, rate)
	eq.ProcessInPlace(pcm)
	after := goertzel(leftChannel(pcm), 1000, rate)
	ratio := after / before
	want := math.Pow(10, 12.0/20) // ≈3.98
	if math.Abs(ratio-want)/want > 0.15 {
		t.Fatalf("1kHz 增益比 = %.3f，期望 ≈%.3f", ratio, want)
	}
}

func TestOutOfBandUnaffected(t *testing.T) {
	const rate = 44100
	bands := make([]float64, BandCount)
	bands[5] = 12 // 只抬 1kHz
	eq := NewEQ(rate, 2, bands, 0)
	pcm := tone(8000, rate, rate/2, 0.05)
	before := goertzel(leftChannel(pcm), 8000, rate)
	eq.ProcessInPlace(pcm)
	after := goertzel(leftChannel(pcm), 8000, rate)
	ratio := after / before
	if math.Abs(ratio-1) > 0.10 {
		t.Fatalf("8kHz 被 1kHz 段影响：比值 %.3f", ratio)
	}
}

func TestPreampPreventsClipping(t *testing.T) {
	const rate = 44100
	bands := make([]float64, BandCount)
	bands[5] = 12
	pre := ComputePreamp(bands)
	if pre >= 0 {
		t.Fatalf("有正增益时前级必须为负，得到 %.2f", pre)
	}
	eq := NewEQ(rate, 2, bands, pre)
	pcm := tone(1000, rate, rate/2, 0.99) // 接近满幅
	eq.ProcessInPlace(pcm)
	peak := 0.0
	for _, v := range leftChannel(pcm) {
		if math.Abs(v) > peak {
			peak = math.Abs(v)
		}
	}
	if peak > 32767 {
		t.Fatalf("削波：峰值 %.0f", peak)
	}
	if peak < 30000 {
		t.Fatalf("前级衰减过度：峰值 %.0f", peak)
	}
}

func TestSampleCountPreserved(t *testing.T) {
	bands := make([]float64, BandCount)
	bands[0] = 6
	eq := NewEQ(44100, 2, bands, -6)
	pcm := tone(440, 44100, 883, 0.3) // 非整块长度
	before := len(pcm)
	eq.ProcessInPlace(pcm)
	if len(pcm) != before {
		t.Fatalf("样本数变了：%d → %d", before, len(pcm))
	}
}

func TestSetBandsRamps(t *testing.T) {
	const rate = 44100
	eq := NewEQ(rate, 2, make([]float64, BandCount), 0)
	bands := make([]float64, BandCount)
	bands[5] = 12
	eq.SetBands(bands, 0)

	// 第一块 20ms 内应当已经接近目标，不是瞬间跳变也不是永不生效。
	first := tone(1000, rate, rate/50, 0.05)
	base := goertzel(leftChannel(first), 1000, rate)
	eq.ProcessInPlace(first)
	after := goertzel(leftChannel(first), 1000, rate)
	ratio := after / base
	if ratio < 1.5 {
		t.Fatalf("20ms 后增益仍未生效：%.3f", ratio)
	}
	if ratio > 4.2 {
		t.Fatalf("增益超过目标（%.3f），说明没有限幅", ratio)
	}
}

func TestChannelsIndependent(t *testing.T) {
	// 右声道喂静音、左声道喂正弦：处理不得把右声道泄漏成有信号。
	const rate = 44100
	bands := make([]float64, BandCount)
	bands[5] = 12
	eq := NewEQ(rate, 2, bands, 0)
	pcm := tone(1000, rate, 4410, 0.5)
	for i := 0; i < 4410; i++ {
		binary.LittleEndian.PutUint16(pcm[i*4+2:], 0)
	}
	eq.ProcessInPlace(pcm)
	for i := 0; i < 4410; i++ {
		if v := int16(binary.LittleEndian.Uint16(pcm[i*4+2:])); v != 0 {
			t.Fatalf("右声道出现信号 %d", v)
		}
	}
}
