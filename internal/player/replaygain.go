package player

import (
	"context"
	"math"
)

// ReplayGainMode ReplayGain 工作模式。
//
// 默认 off：不读标签、不乘增益，行为与没有这个功能时完全一致。
type ReplayGainMode string

const (
	// ReplayGainOff 关闭。
	ReplayGainOff ReplayGainMode = "off"
	// ReplayGainTrack 按单曲增益。
	ReplayGainTrack ReplayGainMode = "track"
	// ReplayGainAlbum 按专辑增益（专辑内响度关系保持不变）。
	ReplayGainAlbum ReplayGainMode = "album"
)

// Valid 模式是否合法。
func (m ReplayGainMode) Valid() bool {
	switch m {
	case ReplayGainOff, ReplayGainTrack, ReplayGainAlbum:
		return true
	}
	return false
}

// ReplayGain 一首歌的 ReplayGain 标签值。
//
// 全部来自 `REPLAYGAIN_*` 标签：gain 是 dB（可为负），peak 是线性峰值（0 表示没有）。
// 缺失的字段为 0，由 GainDB 决定能否使用。
type ReplayGain struct {
	TrackGain float64 `json:"track_gain"`
	TrackPeak float64 `json:"track_peak"`
	AlbumGain float64 `json:"album_gain"`
	AlbumPeak float64 `json:"album_peak"`
}

// Empty 没有任何可用增益。
func (r ReplayGain) Empty() bool {
	return r.TrackGain == 0 && r.AlbumGain == 0
}

// GainDB 按模式选出增益（dB）。
//
// 规则：
//   - off 或没有对应模式的增益 → 0，调用方据此跳过增益；
//   - album 模式缺 album 增益时退回 track 增益（比整张专辑突然变响好）；
//   - preventClipping 且有峰值时，把增益压到 peak×factor ≤ 1.0，宁可少提也不削波。
func (r ReplayGain) GainDB(mode ReplayGainMode, preventClipping bool) float64 {
	var gain, peak float64
	switch mode {
	case ReplayGainTrack:
		gain, peak = r.TrackGain, r.TrackPeak
	case ReplayGainAlbum:
		gain, peak = r.AlbumGain, r.AlbumPeak
		if gain == 0 {
			gain, peak = r.TrackGain, r.TrackPeak
		}
	default:
		return 0
	}
	if gain == 0 {
		return 0
	}
	if preventClipping && peak > 0 {
		if maxDB := -20 * math.Log10(peak); gain > maxDB {
			gain = maxDB
		}
	}
	return gain
}

// GainFactor dB 转线性倍数。
func GainFactor(db float64) float64 {
	if db == 0 {
		return 1
	}
	return math.Pow(10, db/20)
}

// ApplyGain 对 s16le 交错样本就地乘增益，超界钳制到 int16 范围。
//
// 不做抖动、不做限幅：ReplayGain 的削波由 GainDB 的峰值限制提前避免，
// 这里的钳制只是最后一道保险（峰值标签缺失或标错时不至于溢出成噪音）。
func ApplyGain(pcm []byte, factor float64) {
	if factor == 1 || len(pcm) < 2 {
		return
	}
	for i := 0; i+1 < len(pcm); i += 2 {
		v := int32(int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8))
		scaled := int32(math.Round(float64(v) * factor))
		if scaled > math.MaxInt16 {
			scaled = math.MaxInt16
		} else if scaled < math.MinInt16 {
			scaled = math.MinInt16
		}
		pcm[i] = byte(uint16(scaled))
		pcm[i+1] = byte(uint16(scaled) >> 8)
	}
}

// ReplayGainSource 提供一首歌在当前设置下的增益（dB）。
//
// 实现在 app 层（读配置 + ffprobe + 缓存）；player 包只认这个接口，
// 不依赖配置服务，也不依赖 ffprobe 是否可用。ok=false 表示不加增益。
type ReplayGainSource interface {
	GainDB(ctx context.Context, songID int64) (float64, bool)
}

// gainSetter 由分发器实现：Run 之前设置线性增益。
type gainSetter interface {
	SetGain(factor float64)
}
