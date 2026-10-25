// Package dsp 是逐样本的音频处理（目前只有 EQ）。
//
// 为什么不用 ffmpeg 滤镜：一个 ffmpeg 进程只能挂一条滤镜链，每台音箱要独立 EQ
// 就得为每台各起一个解码进程，单解码源多路分发的地基就塌了（同步无从谈起）。
// 所以 EQ 放在分发之后、写设备之前，对每一路目标独立处理。
//
// 处理对象是 s16le 交错 PCM，样本数严格不变（写帧协议依赖这个前提）。
package dsp

import (
	"encoding/binary"
	"math"
	"sync"
)

// BandCount 段数，与客户端 /settings/equalizer 一致。
const BandCount = 10

// CenterFreqs 10 段中心频率（Hz）。
var CenterFreqs = [BandCount]float64{31, 62, 125, 250, 500, 1000, 2000, 4000, 8000, 16000}

// FilterQ peaking 滤波器的 Q。固定 1.0 与旧 ffmpeg equalizer=t=q:w=1.0 一致，
// 换实现不换听感，用户的旧预设不会突然变形。
const FilterQ = 1.0

const (
	// RampMs 参数变更的平滑时长。20ms = 一帧，刚好盖住一次写帧的粒度。
	RampMs = 20
	// SubBlockMs 系数刷新粒度。5ms 一次，10 段 × 2 声道重算代价可忽略。
	SubBlockMs = 5
	// MaxGainDB 单段增益上限，与旧实现（ffmpeg equalizer 合理范围）一致。
	MaxGainDB = 12
)

// biquad 二阶节（Direct Form I）。
type biquad struct {
	b0, b1, b2 float64
	a1, a2     float64
	x1, x2     float64
	y1, y2     float64
}

// setPeaking 按 RBJ Audio EQ Cookbook 设置 peaking 节。
func (b *biquad) setPeaking(sampleRate, f0, q, gainDB float64) {
	if gainDB == 0 {
		*b = biquad{}
		b.b0 = 1
		return
	}
	a := math.Pow(10, gainDB/40)
	w0 := 2 * math.Pi * f0 / sampleRate
	// 中心频率高于奈奎斯特时 peaking 无意义，直接直通。
	if w0 >= math.Pi {
		*b = biquad{}
		b.b0 = 1
		return
	}
	alpha := math.Sin(w0) / (2 * q)
	cosw := math.Cos(w0)
	b0 := 1 + alpha*a
	b1 := -2 * cosw
	b2 := 1 - alpha*a
	a0 := 1 + alpha/a
	a1 := -2 * cosw
	a2 := 1 - alpha/a
	// 保留滤波器状态（改参数时不清零，避免爆音）。
	*b = biquad{
		b0: b0 / a0, b1: b1 / a0, b2: b2 / a0,
		a1: a1 / a0, a2: a2 / a0,
		x1: b.x1, x2: b.x2, y1: b.y1, y2: b.y2,
	}
}

func (b *biquad) process(x float64) float64 {
	y := b.b0*x + b.b1*b.x1 + b.b2*b.x2 - b.a1*b.y1 - b.a2*b.y2
	b.x2, b.x1 = b.x1, x
	b.y2, b.y1 = b.y1, y
	return y
}

// EQ 一条目标通道上的 10 段均衡器。
//
// 并发：ProcessInPlace 由该路的写协程调用，SetBands 由 API 协程调用，
// 用互斥锁保护参数与滤波器状态。
type EQ struct {
	sampleRate int
	channels   int

	mu      sync.Mutex
	cur     [BandCount]float64
	target  [BandCount]float64
	curPre  float64
	tgtPre  float64
	stages  [2][BandCount]biquad
	applied bool
}

// NewEQ 创建均衡器。bands 长度不足补 0，超出的截断；增益钳到 ±12dB。
func NewEQ(sampleRate, channels int, bands []float64, preamp float64) *EQ {
	if sampleRate <= 0 {
		sampleRate = 44100
	}
	if channels <= 0 || channels > 2 {
		channels = 2
	}
	e := &EQ{sampleRate: sampleRate, channels: channels}
	e.target = normalize(bands)
	e.tgtPre = clampPreamp(preamp)
	// 初值直接到位，避免起播第一帧从 0dB 爬上来。
	e.cur = e.target
	e.curPre = e.tgtPre
	return e
}

// SetBands 更新参数。增益在 RampMs 内线性过渡到新值，不产生爆音。
func (e *EQ) SetBands(bands []float64, preamp float64) {
	next := normalize(bands)
	pre := clampPreamp(preamp)
	e.mu.Lock()
	e.target = next
	e.tgtPre = pre
	e.mu.Unlock()
}

// Flat 当前与目标都是直通（决定能否跳过处理）。
func (e *EQ) Flat() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.flatLocked()
}

func (e *EQ) flatLocked() bool {
	if e.curPre != 0 || e.tgtPre != 0 {
		return false
	}
	for i := range e.cur {
		if e.cur[i] != 0 || e.target[i] != 0 {
			return false
		}
	}
	return true
}

// ProcessInPlace 就地处理 s16le 交错 PCM。样本数不变。
func (e *EQ) ProcessInPlace(pcm []byte) {
	frameBytes := 2 * e.channels
	frames := len(pcm) / frameBytes
	if frames == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.flatLocked() {
		return
	}

	step := e.sampleRate * SubBlockMs / 1000
	if step <= 0 {
		step = 1
	}
	for off := 0; off < frames; off += step {
		n := frames - off
		if n > step {
			n = step
		}
		e.advanceLocked(n)
		preLin := math.Pow(10, e.curPre/20)
		for i := 0; i < n; i++ {
			base := (off + i) * frameBytes
			for ch := 0; ch < e.channels; ch++ {
				x := float64(int16(binary.LittleEndian.Uint16(pcm[base+ch*2:]))) * preLin
				for k := range e.stages[ch] {
					x = e.stages[ch][k].process(x)
				}
				if x > 32767 {
					x = 32767
				} else if x < -32768 {
					x = -32768
				}
				binary.LittleEndian.PutUint16(pcm[base+ch*2:], uint16(int16(x)))
			}
		}
	}
}

// advanceLocked 把当前参数朝目标推进 n 个采样的量，并刷新系数。
func (e *EQ) advanceLocked(n int) {
	ramp := e.sampleRate * RampMs / 1000
	if ramp <= 0 {
		ramp = 1
	}
	frac := float64(n) / float64(ramp)
	if frac > 1 {
		frac = 1
	}
	changed := false
	for i := range e.cur {
		d := e.target[i] - e.cur[i]
		if d == 0 {
			continue
		}
		if math.Abs(d) < 0.01 {
			e.cur[i] = e.target[i]
		} else {
			e.cur[i] += d * frac
		}
		changed = true
	}
	if d := e.tgtPre - e.curPre; d != 0 {
		if math.Abs(d) < 0.01 {
			e.curPre = e.tgtPre
		} else {
			e.curPre += d * frac
		}
		changed = true
	}
	if !changed && e.applied {
		return
	}
	e.applied = true
	for ch := 0; ch < e.channels; ch++ {
		for k := 0; k < BandCount; k++ {
			e.stages[ch][k].setPeaking(float64(e.sampleRate), CenterFreqs[k], FilterQ, e.cur[k])
		}
	}
}

func normalize(bands []float64) [BandCount]float64 {
	var out [BandCount]float64
	for i := range out {
		if i >= len(bands) {
			break
		}
		g := bands[i]
		if math.IsNaN(g) || math.IsInf(g, 0) {
			g = 0
		}
		if g > MaxGainDB {
			g = MaxGainDB
		}
		if g < -MaxGainDB {
			g = -MaxGainDB
		}
		out[i] = g
	}
	return out
}

func clampPreamp(p float64) float64 {
	if math.IsNaN(p) || math.IsInf(p, 0) || p > 0 {
		return 0
	}
	if p < -24 {
		return -24
	}
	return p
}

// ComputePreamp 前级衰减（dB，≤0）：只看正增益，留 0.1dB 余量给滤波器过冲。
func ComputePreamp(bands []float64) float64 {
	maxGain := 0.0
	for _, b := range bands {
		if b > maxGain {
			maxGain = b
		}
	}
	if maxGain <= 0 {
		return 0
	}
	return -math.Ceil((maxGain+0.1)*100) / 100
}

// NormalizeBands 把增益钳到 ±12dB，长度固定 10 段。
func NormalizeBands(bands []float64) []float64 {
	n := normalize(bands)
	out := make([]float64, BandCount)
	copy(out, n[:])
	return out
}
