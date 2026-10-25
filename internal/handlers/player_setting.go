package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
)

// 播放器相关配置走业务端点（不是通用 /configs/{key}）：
// 强类型、默认值在 handler 内、副作用也在这里触发。

const (
	playerDiscoveryKey = "player_discovery"
	playerOutputKey    = "player_output"
)

// PlayerDiscoverySetting 设备发现配置。
type PlayerDiscoverySetting struct {
	Enabled         bool     `json:"enabled"`
	Protocols       []string `json:"protocols"`
	IntervalSeconds int      `json:"interval_seconds"`
}

func defaultPlayerDiscovery() PlayerDiscoverySetting {
	return PlayerDiscoverySetting{Enabled: true, Protocols: []string{"raop", "dlna"}, IntervalSeconds: 30}
}

// PlayerOutputSetting 输出与派对配置。
type PlayerOutputSetting struct {
	MaxGroups                  int  `json:"max_groups"`
	DefaultLatencyMs           int  `json:"default_latency_ms"`
	DLNARealignIntervalMinutes int  `json:"dlna_realign_interval_minutes"`
	AllowPrivateNetwork        bool `json:"allow_private_network"`
	PartyEnabled               bool `json:"party_enabled"`
	PartyDriftToleranceMs      int  `json:"party_drift_tolerance_ms"`
}

func defaultPlayerOutput() PlayerOutputSetting {
	return PlayerOutputSetting{
		MaxGroups:                  4,
		DefaultLatencyMs:           2000,
		DLNARealignIntervalMinutes: 0,
		AllowPrivateNetwork:        true,
		PartyEnabled:               true,
		PartyDriftToleranceMs:      1500,
	}
}

// GetPlayerDiscoverySetting 读取设备发现配置
// @Summary 获取设备发现配置
// @Description 返回音箱自动发现配置。enabled 控制是否周期扫描；protocols 可选 raop / dlna；interval_seconds 范围 10-600。未配置时返回默认值（开启、两种协议、30 秒）。
// @Tags 设置
// @Produce json
// @Success 200 {object} handlers.PlayerDiscoverySetting "设备发现配置"
// @Security BearerAuth
// @Router /settings/player-discovery [get]
func (h *ConfigHandler) GetPlayerDiscoverySetting(w http.ResponseWriter, r *http.Request) {
	cfg := defaultPlayerDiscovery()
	if err := h.configService.GetJSON(playerDiscoveryKey, &cfg); err != nil {
		respondJSON(w, http.StatusOK, defaultPlayerDiscovery())
		return
	}
	respondJSON(w, http.StatusOK, cfg)
}

// UpdatePlayerDiscoverySetting 保存设备发现配置
// @Summary 保存设备发现配置
// @Description 保存后立即生效：下一次扫描周期使用新配置。protocols 只接受 raop 与 dlna；interval_seconds 超出 10-600 返回 400。
// @Tags 设置
// @Accept json
// @Produce json
// @Param request body handlers.PlayerDiscoverySetting true "设备发现配置"
// @Success 200 {object} handlers.PlayerDiscoverySetting "保存后的配置"
// @Failure 400 {object} map[string]string "参数错误"
// @Failure 500 {object} map[string]string "保存失败"
// @Security BearerAuth
// @Router /settings/player-discovery [put]
func (h *ConfigHandler) UpdatePlayerDiscoverySetting(w http.ResponseWriter, r *http.Request) {
	var req PlayerDiscoverySetting
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	if req.IntervalSeconds == 0 {
		req.IntervalSeconds = 30
	}
	if req.IntervalSeconds < 10 || req.IntervalSeconds > 600 {
		respondError(w, http.StatusBadRequest, "interval_seconds 必须在 10-600 之间", nil)
		return
	}
	if len(req.Protocols) == 0 {
		req.Protocols = []string{"raop", "dlna"}
	}
	for _, p := range req.Protocols {
		if p != "raop" && p != "dlna" {
			respondError(w, http.StatusBadRequest, "protocols 只支持 raop / dlna", nil)
			return
		}
	}
	if err := h.configService.SetJSON(playerDiscoveryKey, req); err != nil {
		respondError(w, http.StatusInternalServerError, "保存配置失败", err)
		return
	}
	// 副作用与通用 PUT /configs/player_discovery 一致：立即让发现器换配置。
	if h.onConfigChanged != nil {
		go h.onConfigChanged(playerDiscoveryKey)
	}
	respondJSON(w, http.StatusOK, req)
}

// GetPlayerOutputSetting 读取输出配置
// @Summary 获取播放输出配置
// @Description 返回并发解码组数上限、默认起播延迟、DLNA 重对齐间隔、是否允许访问私网、派对开关与漂移容忍。未配置时返回默认值。
// @Tags 设置
// @Produce json
// @Success 200 {object} handlers.PlayerOutputSetting "输出配置"
// @Security BearerAuth
// @Router /settings/player-output [get]
func (h *ConfigHandler) GetPlayerOutputSetting(w http.ResponseWriter, r *http.Request) {
	cfg := defaultPlayerOutput()
	if err := h.configService.GetJSON(playerOutputKey, &cfg); err != nil {
		respondJSON(w, http.StatusOK, defaultPlayerOutput())
		return
	}
	respondJSON(w, http.StatusOK, cfg)
}

// UpdatePlayerOutputSetting 保存输出配置
// @Summary 保存播放输出配置
// @Description 保存后立即生效：max_groups 调整解码预算（已在播的组不被打断，只影响后续申请）；party_drift_tolerance_ms 必须在 500-5000，越界返回 400；max_groups 必须在 1-16。
// @Tags 设置
// @Accept json
// @Produce json
// @Param request body handlers.PlayerOutputSetting true "输出配置"
// @Success 200 {object} handlers.PlayerOutputSetting "保存后的配置"
// @Failure 400 {object} map[string]string "参数错误"
// @Failure 500 {object} map[string]string "保存失败"
// @Security BearerAuth
// @Router /settings/player-output [put]
func (h *ConfigHandler) UpdatePlayerOutputSetting(w http.ResponseWriter, r *http.Request) {
	var req PlayerOutputSetting
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	if req.MaxGroups == 0 {
		req.MaxGroups = 4
	}
	if req.MaxGroups < 1 || req.MaxGroups > 16 {
		respondError(w, http.StatusBadRequest, "max_groups 必须在 1-16 之间", nil)
		return
	}
	if req.DefaultLatencyMs <= 0 {
		req.DefaultLatencyMs = 2000
	}
	if req.PartyDriftToleranceMs == 0 {
		req.PartyDriftToleranceMs = 1500
	}
	if req.PartyDriftToleranceMs < 500 || req.PartyDriftToleranceMs > 5000 {
		respondError(w, http.StatusBadRequest, "party_drift_tolerance_ms 必须在 500-5000 之间", nil)
		return
	}
	if req.DLNARealignIntervalMinutes < 0 {
		req.DLNARealignIntervalMinutes = 0
	}
	if err := h.configService.SetJSON(playerOutputKey, req); err != nil {
		respondError(w, http.StatusInternalServerError, "保存配置失败", err)
		return
	}
	respondJSON(w, http.StatusOK, req)
}

// ParseProtocols 把协议列表规范化为小写集合，供发现器使用。
func ParseProtocols(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, p := range list {
		out[strings.ToLower(strings.TrimSpace(p))] = true
	}
	return out
}
