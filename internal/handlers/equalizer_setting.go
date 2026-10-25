package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"

	"songloft/internal/player"
)

const equalizerKey = "equalizer"

// equalizerSetting 均衡器配置
type equalizerSetting struct {
	Enabled bool      `json:"enabled"`
	Preset  string    `json:"preset"`
	Bands   []float64 `json:"bands"`
}

var defaultEqualizerSetting = equalizerSetting{
	Enabled: false,
	Preset:  "flat",
	Bands:   []float64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
}

// GetEqualizerSetting 获取均衡器配置
// @Summary 获取均衡器配置
// @Description 获取全局均衡器（EQ）配置，包含启用状态、预设名称和 10 段频段增益（31Hz–16kHz，单位 dB，范围 -12 ~ +12）。未配置时返回默认值（关闭 + flat 预设 + 全 0）。
// @Tags 设置
// @Produce json
// @Success 200 {object} equalizerSetting "均衡器配置"
// @Security BearerAuth
// @Router /settings/equalizer [get]
func (h *ConfigHandler) GetEqualizerSetting(w http.ResponseWriter, r *http.Request) {
	var cfg equalizerSetting
	if err := h.configService.GetJSON(equalizerKey, &cfg); err != nil {
		respondJSON(w, http.StatusOK, defaultEqualizerSetting)
		return
	}
	respondJSON(w, http.StatusOK, cfg)
}

// UpdateEqualizerSetting 保存均衡器配置
// @Summary 保存均衡器配置
// @Description 保存全局均衡器（EQ）配置。bands 必须包含 10 个元素，每个值在 -12 ~ +12 范围内（单位 dB）。preset 为预设名称，取值与 GET /players/{id}/eq/presets 返回的 id 一致（flat/pop/rock/jazz/classical/vocal/electronic/bass_boost/treble_boost/loudness），或 custom 表示自定义曲线；未知预设返回 400。
// @Tags 设置
// @Accept json
// @Produce json
// @Param request body equalizerSetting true "均衡器配置"
// @Success 200 {object} equalizerSetting "保存后的均衡器配置"
// @Failure 400 {object} models.ErrorResponse "请求格式错误"
// @Failure 500 {object} models.ErrorResponse "保存配置失败"
// @Security BearerAuth
// @Router /settings/equalizer [put]
func (h *ConfigHandler) UpdateEqualizerSetting(w http.ResponseWriter, r *http.Request) {
	var req equalizerSetting
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	// 预设名与 GET /players/{id}/eq/presets 同一份表，避免两套常量漂移。
	// "custom" 表示用户自己拖的曲线，没有对应预设。
	if req.Preset != "" && req.Preset != "custom" {
		if _, ok := player.EQPresetByID(req.Preset); !ok {
			respondError(w, http.StatusBadRequest, "未知的预设："+req.Preset, nil)
			return
		}
	}
	if len(req.Bands) != 10 {
		respondError(w, http.StatusBadRequest, "bands 必须包含 10 个元素", nil)
		return
	}
	for i, gain := range req.Bands {
		if gain < -12 || gain > 12 {
			respondError(w, http.StatusBadRequest, "bands["+strconv.Itoa(i)+"] 超出范围 -12 ~ +12", nil)
			return
		}
	}
	if err := h.configService.SetJSON(equalizerKey, req); err != nil {
		respondError(w, http.StatusInternalServerError, "保存配置失败", err)
		return
	}
	respondJSON(w, http.StatusOK, req)
}
