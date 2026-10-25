package pipeline

import (
	"fmt"
	"math"
	"strings"

	"songloft/internal/player"
)

// EQCenterFreqs 10 段中心频率，与客户端 /settings/equalizer 的预设一致。
var EQCenterFreqs = [10]float64{31, 62, 125, 250, 500, 1000, 2000, 4000, 8000, 16000}

// BuildFilterChain 把 EQProfile 翻译成 ffmpeg -af 链。
//
// 两个必须点：
//  1. 每个 band 用 peaking equalizer（`t=q`），Q 固定 1.0；
//  2. 前级衰减 = -max(0, max(band))，避免正增益把 0dBFS 顶穿成削波。
//     preamp 由 ComputePreamp 算，这里用调用方传进来的值（存在 players.eq_profile）。
//
// 返回空串表示不需要处理（flat），此时调用方可以走直通不占解码槽。
func BuildFilterChain(eq player.EQProfile) string {
	if eq.Flat() {
		return ""
	}
	parts := make([]string, 0, len(EQCenterFreqs)+1)
	if eq.Preamp < 0 {
		parts = append(parts, fmt.Sprintf("volume=%.2fdB", eq.Preamp))
	}
	for i, f := range EQCenterFreqs {
		g := 0.0
		if i < len(eq.Bands) {
			g = eq.Bands[i]
		}
		if g == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("equalizer=f=%.0f:t=q:w=1.0:g=%.2f", f, g))
	}
	return strings.Join(parts, ",")
}

// ComputePreamp 计算前级衰减（dB，≤0）。
//
// 只看正增益：负增益不会削波，无需补偿。留 0.1dB 余量给滤波器过冲。
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

// NormalizeBands 把增益钳到 ±12dB（ffmpeg equalizer 的合理范围），
// 长度固定 10 段，短的补 0、长的截断。
func NormalizeBands(bands []float64) []float64 {
	out := make([]float64, player.EQBandCount)
	for i := range out {
		if i >= len(bands) {
			break
		}
		g := bands[i]
		if g > 12 {
			g = 12
		}
		if g < -12 {
			g = -12
		}
		out[i] = g
	}
	return out
}
