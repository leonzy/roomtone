package player

import "songloft/internal/player/dsp"

// EQPreset 一组 10 段均衡器预设。
//
// 预设只是"填好 Bands 再 PUT /players/{id}/eq"的便捷起点：服务端不记住
// 用户用了哪个预设，也不在预设变更时改写已有音箱的 EQ——用户微调过的
// 曲线不能被一次预设升级悄悄覆盖。
type EQPreset struct {
	ID   string `json:"id" example:"rock"`
	Name string `json:"name" example:"摇滚"`
	// Bands 10 段增益（dB），顺序对应 dsp.CenterFreqs。
	Bands []float64 `json:"bands"`
	// Preamp 服务端按最大正增益算出的前级衰减（dB，≤0），前端直接用。
	Preamp float64 `json:"preamp" example:"-5.1"`
}

// eqPresetTable 预设表。增益以 31/62/125/250/500/1k/2k/4k/8k/16k Hz 排列，
// 与客户端 /settings/equalizer 的 10 段保持一致。
var eqPresetTable = []struct {
	id    string
	name  string
	bands [dsp.BandCount]float64
}{
	{"flat", "原声", [dsp.BandCount]float64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
	{"pop", "流行", [dsp.BandCount]float64{-1.5, -1, 0, 2, 4, 4, 2, 0, -1, -1.5}},
	{"rock", "摇滚", [dsp.BandCount]float64{5, 4, 3, 1.5, -1, -1, 0.5, 2.5, 3.5, 4.5}},
	{"jazz", "爵士", [dsp.BandCount]float64{4, 3, 2, 2, -1, -1, 0, 2, 3, 4}},
	{"classical", "古典", [dsp.BandCount]float64{4, 3, 2, 1, -1, -1, 0, 2, 3, 4}},
	{"vocal", "人声", [dsp.BandCount]float64{-2, -1.5, 0, 2, 4, 4, 3, 1, 0, -1}},
	{"electronic", "电子", [dsp.BandCount]float64{5, 4, 3, 1, 0, -1, 0, 1.5, 3, 4}},
	{"bass_boost", "低频增强", [dsp.BandCount]float64{7, 6, 4, 2, 0, 0, 0, 0, 0, 0}},
	{"treble_boost", "高频增强", [dsp.BandCount]float64{0, 0, 0, 0, 0, 1, 3, 5, 6, 7}},
	{"loudness", "响度", [dsp.BandCount]float64{6, 4, 0, -2, -1, 0, 1, 3, 5, 6}},
}

// EQPresets 返回内置预设。每次返回新切片，调用方可以随意修改。
func EQPresets() []EQPreset {
	out := make([]EQPreset, 0, len(eqPresetTable))
	for _, p := range eqPresetTable {
		bands := dsp.NormalizeBands(p.bands[:])
		out = append(out, EQPreset{
			ID:     p.id,
			Name:   p.name,
			Bands:  bands,
			Preamp: dsp.ComputePreamp(bands),
		})
	}
	return out
}

// EQPresetByID 按 id 取预设。
func EQPresetByID(id string) (EQPreset, bool) {
	for _, p := range EQPresets() {
		if p.ID == id {
			return p, true
		}
	}
	return EQPreset{}, false
}
