package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"songloft/internal/services"
)

// 播放器相关配置走业务端点（不是通用 /configs/{key}）：
// 强类型、默认值在 handler 内、副作用也在这里触发。

const (
	playerDiscoveryKey = "player_discovery"
	playerOutputKey    = "player_output"
	crossfadeKey       = "crossfade"
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
	// NetworkSinksEnabled 是否允许网络音箱（RAOP / DLNA）。
	//
	// 服务器部署默认 true；iOS Bundle 恒为 false（由宿主强制覆盖）；
	// 桌面 Bundle 需要用户显式开启。关闭后手动添加与重连都返回 403 并说明原因。
	NetworkSinksEnabled   bool `json:"network_sinks_enabled"`
	PartyEnabled          bool `json:"party_enabled"`
	PartyDriftToleranceMs int  `json:"party_drift_tolerance_ms"`
}

func defaultPlayerOutput() PlayerOutputSetting {
	return PlayerOutputSetting{
		MaxGroups:                  4,
		DefaultLatencyMs:           2000,
		DLNARealignIntervalMinutes: 0,
		AllowPrivateNetwork:        true,
		NetworkSinksEnabled:        true,
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
	// 先取旧值再解请求体：老客户端（或 curl 手写 body）不带
	// network_sinks_enabled 时，不能因为零值把网络音箱全关掉。
	req := defaultPlayerOutput()
	if err := h.configService.GetJSON(playerOutputKey, &req); err != nil {
		req = defaultPlayerOutput()
	}
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

const playerOutputAllowlistKey = "player_output_allowlist"

// PlayerOutputAllowlistSetting 播放器出口白名单。
//
// 与代理白名单（proxy_private_allowlist）是两套配置，互不影响：
// 前者决定"能往哪些网段推音频"，后者决定"服务端出站 HTTP 能去哪"。
// entries 为空表示不限制（私网全允许），非空则收敛到这些网段；公网恒拒绝。
type PlayerOutputAllowlistSetting struct {
	Entries []string `json:"entries"`
}

// GetPlayerOutputAllowlistSetting 读取播放器出口白名单
// @Summary 获取播放器出口白名单
// @Description 返回允许推送音频的网段（CIDR 或单个 IP）。空数组表示不限制私网；配置后只允许这些网段，公网地址恒拒绝。与代理白名单互不影响。
// @Tags 设置
// @Produce json
// @Success 200 {object} handlers.PlayerOutputAllowlistSetting "白名单"
// @Security BearerAuth
// @Router /settings/player-output-allowlist [get]
func (h *ConfigHandler) GetPlayerOutputAllowlistSetting(w http.ResponseWriter, r *http.Request) {
	cfg := PlayerOutputAllowlistSetting{Entries: []string{}}
	if err := h.configService.GetJSON(playerOutputAllowlistKey, &cfg); err != nil {
		respondJSON(w, http.StatusOK, PlayerOutputAllowlistSetting{Entries: []string{}})
		return
	}
	if cfg.Entries == nil {
		cfg.Entries = []string{}
	}
	respondJSON(w, http.StatusOK, cfg)
}

// UpdatePlayerOutputAllowlistSetting 保存播放器出口白名单
// @Summary 保存播放器出口白名单
// @Description 保存后立即生效（下一次手动添加设备或重连时校验）。条目支持 CIDR 与单个 IP，非法条目返回 400。只影响播放器出口，不改动代理白名单。
// @Tags 设置
// @Accept json
// @Produce json
// @Param request body handlers.PlayerOutputAllowlistSetting true "白名单"
// @Success 200 {object} handlers.PlayerOutputAllowlistSetting "保存后的白名单"
// @Failure 400 {object} map[string]string "条目格式错误"
// @Failure 500 {object} map[string]string "保存失败"
// @Security BearerAuth
// @Router /settings/player-output-allowlist [put]
func (h *ConfigHandler) UpdatePlayerOutputAllowlistSetting(w http.ResponseWriter, r *http.Request) {
	var req PlayerOutputAllowlistSetting
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	if req.Entries == nil {
		req.Entries = []string{}
	}
	if _, err := services.ParsePlayerOutputAllowlist(req.Entries); err != nil {
		respondError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}
	if err := h.configService.SetJSON(playerOutputAllowlistKey, req); err != nil {
		respondError(w, http.StatusInternalServerError, "保存配置失败", err)
		return
	}
	if h.onConfigChanged != nil {
		go h.onConfigChanged(playerOutputAllowlistKey)
	}
	respondJSON(w, http.StatusOK, req)
}

// ReplayGainKey ReplayGain 配置的 KV 键（业务端点专用，不接受通用 /configs 写入）。
const ReplayGainKey = "replaygain"

// ReplayGainSetting ReplayGain 配置。
type ReplayGainSetting struct {
	// Mode off / track / album；默认 off。
	Mode string `json:"mode"`
	// PreventClipping 按标签峰值压低增益，避免提音量后削波；默认 true。
	PreventClipping bool `json:"prevent_clipping"`
}

func defaultReplayGain() ReplayGainSetting {
	return ReplayGainSetting{Mode: "off", PreventClipping: true}
}

// GetReplayGainSetting 读取 ReplayGain 配置
// @Summary 获取 ReplayGain 配置
// @Description 返回响度均衡模式与防削波开关。mode 为 off（默认，完全不处理）/ track（按单曲增益）/ album（按专辑增益，专辑内响度关系保持不变）；prevent_clipping 开启时按 REPLAYGAIN_*_PEAK 压低增益，宁可少提也不削波。未配置时返回 off + 开启。
// @Tags 设置
// @Produce json
// @Success 200 {object} handlers.ReplayGainSetting "ReplayGain 配置"
// @Security BearerAuth
// @Router /settings/replaygain [get]
func (h *ConfigHandler) GetReplayGainSetting(w http.ResponseWriter, r *http.Request) {
	cfg := defaultReplayGain()
	if err := h.configService.GetJSON(ReplayGainKey, &cfg); err != nil {
		respondJSON(w, http.StatusOK, defaultReplayGain())
		return
	}
	respondJSON(w, http.StatusOK, cfg)
}

// UpdateReplayGainSetting 保存 ReplayGain 配置
// @Summary 保存 ReplayGain 配置
// @Description 保存后下一首开始生效（当前曲目的增益不中途改变，避免播放中音量跳变）。mode 只接受 off / track / album，其他值返回 400。标签在每首歌首次播放时用 ffprobe 读取并缓存，因此存量曲库无需重新扫描。
// @Tags 设置
// @Accept json
// @Produce json
// @Param request body handlers.ReplayGainSetting true "ReplayGain 配置"
// @Success 200 {object} handlers.ReplayGainSetting "保存后的配置"
// @Failure 400 {object} map[string]string "mode 非法或请求格式错误"
// @Failure 500 {object} map[string]string "保存失败"
// @Security BearerAuth
// @Router /settings/replaygain [put]
func (h *ConfigHandler) UpdateReplayGainSetting(w http.ResponseWriter, r *http.Request) {
	var req ReplayGainSetting
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	switch req.Mode {
	case "off", "track", "album":
	case "":
		req.Mode = "off"
	default:
		respondError(w, http.StatusBadRequest, "mode 只支持 off / track / album", nil)
		return
	}
	if err := h.configService.SetJSON(ReplayGainKey, req); err != nil {
		respondError(w, http.StatusInternalServerError, "保存配置失败", err)
		return
	}
	if h.onConfigChanged != nil {
		go h.onConfigChanged(ReplayGainKey)
	}
	respondJSON(w, http.StatusOK, req)
}

// CrossfadeSetting 交叉淡化配置。
//
// 与 gapless 互斥（产品决策）：开启后交界处不再走解码器接续，而是把当前曲
// 尾巴与下一曲开头等功率混合。默认关闭，保持原有 gapless 行为。
type CrossfadeSetting struct {
	Enabled bool `json:"enabled"`
	// Seconds 淡化时长，0.5-12 秒；enabled=false 时忽略。
	Seconds float64 `json:"seconds"`
}

func defaultCrossfade() CrossfadeSetting {
	return CrossfadeSetting{Enabled: false, Seconds: 3}
}

// GetCrossfadeSetting 读取交叉淡化配置
// @Summary 获取交叉淡化配置
// @Description 返回交叉淡化（crossfade）设置。enabled=false 时走原有 gapless 接续；enabled=true 时两曲交界按 seconds 秒等功率混合，此时 gapless 自动停用。未配置时返回默认值（关闭、3 秒）。
// @Tags 设置
// @Produce json
// @Success 200 {object} CrossfadeSetting "交叉淡化配置"
// @Security BearerAuth
// @Router /settings/crossfade [get]
func (h *ConfigHandler) GetCrossfadeSetting(w http.ResponseWriter, r *http.Request) {
	cfg := defaultCrossfade()
	if err := h.configService.GetJSON(crossfadeKey, &cfg); err != nil {
		cfg = defaultCrossfade()
	}
	respondJSON(w, http.StatusOK, cfg)
}

// UpdateCrossfadeSetting 更新交叉淡化配置
// @Summary 更新交叉淡化配置
// @Description 写入交叉淡化设置，下一曲起生效（正在播放的曲目不受影响）。seconds 范围 0.5-12；enabled=true 时 gapless 接续自动停用，交界处重新打开解码器（分发器 2s 缓冲吸收开销）。
// @Tags 设置
// @Accept json
// @Produce json
// @Param request body CrossfadeSetting true "交叉淡化配置"
// @Success 200 {object} CrossfadeSetting "更新后的配置"
// @Failure 400 {object} map[string]string "参数非法"
// @Failure 500 {object} map[string]string "写入失败"
// @Security BearerAuth
// @Router /settings/crossfade [put]
func (h *ConfigHandler) UpdateCrossfadeSetting(w http.ResponseWriter, r *http.Request) {
	var cfg CrossfadeSetting
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		respondError(w, http.StatusBadRequest, "请求体解析失败", err)
		return
	}
	if cfg.Enabled && (cfg.Seconds < 0.5 || cfg.Seconds > 12) {
		respondError(w, http.StatusBadRequest, "seconds 必须在 0.5-12 之间", nil)
		return
	}
	if err := h.configService.SetJSON(crossfadeKey, cfg); err != nil {
		respondError(w, http.StatusInternalServerError, "写入交叉淡化配置失败", err)
		return
	}
	respondJSON(w, http.StatusOK, cfg)
}
