package player

import (
	"fmt"
	"slices"
	"strings"
)

// SignalPath 区域信号链路的只读诊断（GET /playback 的 signal_path）。
//
// Kind 是前端三色指示的唯一依据：
//   - lossless：无 EQ、无响度归一，且输出不是有损编码；
//   - enhanced：经过 DSP（EQ / 响度归一），但输出仍是无损；
//   - lossy：输出编码为 AAC / MP3。有损优先于 enhanced，因为用户更需要
//     知道"这一段已经不可逆地损失了信息"。
type SignalPath struct {
	Kind   string        `json:"kind"`
	Stages []SignalStage `json:"stages"`
}

// SignalStage 信号链路中的一段。
type SignalStage struct {
	Name   string `json:"name"`
	Detail string `json:"detail"`
}

// SignalPath.Kind 的取值。
const (
	SignalKindLossless = "lossless"
	SignalKindEnhanced = "enhanced"
	SignalKindLossy    = "lossy"
)

// signalStageNames 固定五段，顺序与前端信号路径面板一致。
var signalStageNames = [...]string{"源", "解码", "DSP", "输出", "设备"}

// buildSignalPath 按组成员、输出格式、源格式与响度开关生成信号路径。
//
// players 是组内已解析的成员；output 是组协商后的 sink 格式；source 是当前
// 曲目的源格式摘要（未知传空串）；loudness 表示组级响度归一是否开启。
func buildSignalPath(players []*Player, output Format, source string, loudness bool) SignalPath {
	kind := SignalKindLossless

	// EQ 是逐成员的：组内任一成员开了非 flat 曲线，整条链路就算经过 DSP。
	// 多个成员用不同预设时合并展示，避免同一段重复写 "EQ: ..."。
	presets := make([]string, 0, len(players))
	for _, p := range players {
		if p.EQ.Flat() {
			continue
		}
		preset := strings.TrimSpace(p.EQ.Preset)
		// preset=flat 但 bands 非零说明用户微调过，按自定义曲线展示。
		if preset == "" || preset == "flat" {
			preset = "custom"
		}
		if !slices.Contains(presets, preset) {
			presets = append(presets, preset)
		}
	}

	dsp := make([]string, 0, 2)
	if len(presets) > 0 {
		dsp = append(dsp, "EQ: "+strings.Join(presets, ", "))
		kind = SignalKindEnhanced
	}
	if loudness {
		dsp = append(dsp, "响度归一")
		kind = SignalKindEnhanced
	}
	if isLossyCodec(output.Codec) {
		kind = SignalKindLossy
	}

	dspDetail := "直通"
	if len(dsp) > 0 {
		dspDetail = strings.Join(dsp, ", ")
	}
	if strings.TrimSpace(source) == "" {
		source = "unknown"
	}

	return SignalPath{
		Kind: kind,
		Stages: []SignalStage{
			{Name: signalStageNames[0], Detail: source},
			{Name: signalStageNames[1], Detail: "ffmpeg"},
			{Name: signalStageNames[2], Detail: dspDetail},
			{Name: signalStageNames[3], Detail: outputFormatDetail(output)},
			{Name: signalStageNames[4], Detail: deviceDetail(players)},
		},
	}
}

// isLossyCodec 输出编码是否有损。当前协商表里只有 AAC / MP3 两档有损。
func isLossyCodec(c Codec) bool {
	return c == CodecAAC || c == CodecMP3
}

// sourceFormatDetail 把曲目源格式拼成 "FLAC 44.1kHz" / "MP3 44.1kHz 320kbps"。
// 格式未知时返回 "unknown"，不要用文件扩展名猜。
func sourceFormatDetail(t Track) string {
	format := strings.ToUpper(strings.TrimSpace(t.Format))
	if format == "" {
		return "unknown"
	}
	detail := format
	if t.SampleRate > 0 {
		detail += " " + formatSampleRate(t.SampleRate)
	}
	if t.BitRate > 0 && isLossyFormatName(format) {
		detail += fmt.Sprintf(" %dkbps", t.BitRate)
	}
	return detail
}

// outputFormatDetail 把协商后的 sink 格式拼成 "L16 44.1kHz/16bit"。
func outputFormatDetail(f Format) string {
	codec := strings.ToUpper(strings.TrimSpace(string(f.Codec)))
	if codec == "" {
		return "unknown"
	}
	detail := codec
	if f.SampleRate > 0 {
		detail += " " + formatSampleRate(f.SampleRate)
	}
	if f.Bits > 0 {
		detail += fmt.Sprintf("/%dbit", f.Bits)
	}
	return detail
}

// formatSampleRate 44100 → "44.1kHz"，48000 → "48kHz"。
func formatSampleRate(rate int) string {
	if rate <= 0 {
		return ""
	}
	if rate%1000 == 0 {
		return fmt.Sprintf("%dkHz", rate/1000)
	}
	return fmt.Sprintf("%.1fkHz", float64(rate)/1000)
}

// isLossyFormatName 源格式名是否有损（用于决定要不要展示码率）。
func isLossyFormatName(format string) bool {
	switch format {
	case "AAC", "MP3", "OGG", "OPUS", "M4A", "WMA":
		return true
	}
	return false
}

// deviceDetail 单成员显示设备名，多成员只显示数量（前端展开后逐成员看）。
func deviceDetail(players []*Player) string {
	switch len(players) {
	case 0:
		return "无输出"
	case 1:
		name := strings.TrimSpace(players[0].Name)
		if name == "" {
			name = players[0].ID
		}
		if name == "" {
			return "unknown"
		}
		return name
	default:
		return fmt.Sprintf("%d 个输出", len(players))
	}
}

// loudnessEnabledLocked 组级响度归一是否开启。
//
// 现有实现是 ReplayGain（/settings/replaygain）；player 包不依赖配置服务，
// 通过可选接口查询。实现方没提供该能力时按关闭处理（跳过该条件）。
// 调用方必须持有 Manager 读锁。
func (m *Manager) loudnessEnabledLocked() bool {
	if m.deps.ReplayGain == nil {
		return false
	}
	src, ok := m.deps.ReplayGain.(LoudnessSource)
	if !ok {
		return false
	}
	return src.LoudnessEnabled()
}
