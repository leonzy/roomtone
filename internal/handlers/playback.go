package handlers

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"songloft/internal/app/ws"
	appmiddleware "songloft/internal/middleware"
	"songloft/internal/player"
	"songloft/internal/player/dsp"
)

// PlaybackHandler 播放控制与音箱管理。
//
// 所有端点都在 /api/v1 下、走 JWT 中间件；错误统一 respondError。
type PlaybackHandler struct {
	mgr *player.Manager
	hub *ws.Hub
	// discovery 设备发现；Bundle / 精简环境可能为 nil。
	discovery DiscoveryRunner
	// allowPrivate 手动添加设备时是否允许私网地址（/settings/player-output）。
	allowPrivate func() bool
	// outputAllowlist 播放器出口白名单（/settings/player-output-allowlist），与代理白名单独立。
	outputAllowlist func() []*net.IPNet
	// networkSinks 当前运行模式是否允许网络音箱（X-04）。
	networkSinks func() (bool, string)
}

// NewPlaybackHandler 创建 handler。
func NewPlaybackHandler(mgr *player.Manager, hub *ws.Hub) *PlaybackHandler {
	return &PlaybackHandler{mgr: mgr, hub: hub}
}

// 把领域错误映射成状态码。文案直接给用户看，所以不打英文错误前缀。
func playbackStatus(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, player.ErrPlayerNotFound), errors.Is(err, player.ErrGroupNotFound), errors.Is(err, player.ErrPartyNotFound):
		return http.StatusNotFound
	case errors.Is(err, player.ErrBudgetExhausted), errors.Is(err, player.ErrBudgetEQCaused):
		return http.StatusServiceUnavailable
	case errors.Is(err, player.ErrPartyDisabled), errors.Is(err, player.ErrNotifyForbidden):
		return http.StatusForbidden
	case errors.Is(err, errNetworkSinkDisabled):
		return http.StatusForbidden
	case errors.Is(err, player.ErrPlayerOffline):
		return http.StatusConflict
	case errors.Is(err, player.ErrGroupInParty), errors.Is(err, player.ErrDuplicateMember):
		return http.StatusConflict
	case errors.Is(err, player.ErrSyncTierMismatch), errors.Is(err, player.ErrQueueEmpty), errors.Is(err, player.ErrQueueIndexOutOfRange):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func playbackFail(w http.ResponseWriter, err error) {
	respondError(w, playbackStatus(err), err.Error(), nil)
}

// ListPlayers 设备列表
// @Summary 列出播放设备
// @Description 返回所有已发现的播放设备：类型（local/raop/dlna）、在线状态、能力、音量与 EQ 配置。前端据此渲染音箱选择器与分组面板。
// @Tags 播放控制
// @Produce json
// @Success 200 {array} player.Player "设备列表"
// @Security BearerAuth
// @Router /players [get]
func (h *PlaybackHandler) ListPlayers(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, h.mgr.ListPlayers())
}

// UpdatePlayer 更新设备
// @Summary 更新播放设备配置
// @Description 修改设备名称或接管行为。takeover_behavior 只对 local 设备生效：pause（默认，被切走后立即暂停并提示）或 follow（静默转为纯控制器）。
// @Tags 播放控制
// @Accept json
// @Produce json
// @Param id path string true "设备 ID"
// @Param request body updatePlayerRequest true "更新内容"
// @Success 200 {object} player.Player "更新后的设备"
// @Failure 400 {object} map[string]string "参数错误"
// @Failure 404 {object} map[string]string "设备不存在"
// @Security BearerAuth
// @Router /players/{id} [put]
func (h *PlaybackHandler) UpdatePlayer(w http.ResponseWriter, r *http.Request) {
	var req updatePlayerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	p, err := h.mgr.UpdatePlayer(r.Context(), chi.URLParam(r, "id"), req.Name, player.TakeoverBehavior(req.TakeoverBehavior))
	if err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, p)
}

type updatePlayerRequest struct {
	Name             string `json:"name"`
	TakeoverBehavior string `json:"takeover_behavior"`
}

// SetVolume 设置音量
// @Summary 设置设备音量
// @Description 设置音量（0-100）与静音状态。支持硬件音量的音箱会立即生效；local 设备通过 WebSocket 下发指令。
// @Tags 播放控制
// @Accept json
// @Produce json
// @Param id path string true "设备 ID"
// @Param request body volumeRequest true "音量"
// @Success 200 {object} player.Player "更新后的设备"
// @Failure 404 {object} map[string]string "设备不存在"
// @Security BearerAuth
// @Router /players/{id}/volume [post]
func (h *PlaybackHandler) SetVolume(w http.ResponseWriter, r *http.Request) {
	var req volumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	p, err := h.mgr.SetVolume(r.Context(), chi.URLParam(r, "id"), req.Volume, req.Muted)
	if err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, p)
}

type volumeRequest struct {
	Volume int  `json:"volume"`
	Muted  bool `json:"muted"`
}

// GetEQ 读取设备 EQ
// @Summary 获取设备均衡器配置
// @Description 返回该音箱的 10 段 EQ 配置与服务端计算的前级衰减 preamp（用于防削波）。
// @Tags 播放控制
// @Produce json
// @Param id path string true "设备 ID"
// @Success 200 {object} player.EQProfile "EQ 配置"
// @Failure 404 {object} map[string]string "设备不存在"
// @Security BearerAuth
// @Router /players/{id}/eq [get]
func (h *PlaybackHandler) GetEQ(w http.ResponseWriter, r *http.Request) {
	p, err := h.mgr.GetPlayer(chi.URLParam(r, "id"))
	if err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, p.EQ)
}

// SetEQ 保存设备 EQ
// @Summary 保存设备均衡器配置
// @Description 保存 10 段 EQ。服务端会把增益钳到 ±12dB、重算 preamp 并回填。任一频段非 0 时该组强制走服务端解码管线，占用一个解码槽位。
// @Tags 播放控制
// @Accept json
// @Produce json
// @Param id path string true "设备 ID"
// @Param request body player.EQProfile true "EQ 配置"
// @Success 200 {object} player.EQProfile "保存后的配置（含 preamp）"
// @Failure 400 {object} map[string]string "参数错误"
// @Failure 404 {object} map[string]string "设备不存在"
// @Security BearerAuth
// @Router /players/{id}/eq [put]
func (h *PlaybackHandler) SetEQ(w http.ResponseWriter, r *http.Request) {
	var req player.EQProfile
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	req.Bands = dsp.NormalizeBands(req.Bands)
	req.Preamp = dsp.ComputePreamp(req.Bands)
	out, err := h.mgr.SetEQ(r.Context(), chi.URLParam(r, "id"), req)
	if err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, out)
}

// ListEQPresets 内置 EQ 预设
// @Summary 列出内置均衡器预设
// @Description 返回 10 段内置预设（原声/流行/摇滚/爵士/古典/人声/电子/低频增强/高频增强/响度），每项都带服务端算好的 preamp。预设是便捷起点：前端选中后自行 PUT /players/{id}/eq 保存，服务端不记录"当前用哪个预设"。设备不存在返回 404。
// @Tags 播放控制
// @Produce json
// @Param id path string true "设备 ID"
// @Success 200 {array} player.EQPreset "预设列表"
// @Failure 404 {object} map[string]string "设备不存在"
// @Security BearerAuth
// @Router /players/{id}/eq/presets [get]
func (h *PlaybackHandler) ListEQPresets(w http.ResponseWriter, r *http.Request) {
	if _, err := h.mgr.GetPlayer(chi.URLParam(r, "id")); err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, player.EQPresets())
}

// ListGroups 组列表
// @Summary 列出同步组
// @Description 返回所有同步组，含成员、主控设备、同步等级（exact/aligned）与当前播放状态。含 DLNA 成员的组会被降级为 aligned，前端必须提示精度下降。
// @Tags 播放控制
// @Produce json
// @Success 200 {array} player.GroupView "同步组列表"
// @Security BearerAuth
// @Router /groups [get]
func (h *PlaybackHandler) ListGroups(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, h.mgr.ListGroups())
}

// GetGroup 组详情
// @Summary 获取同步组详情
// @Description 返回单个组的成员、同步等级与播放状态。
// @Tags 播放控制
// @Produce json
// @Param id path string true "组 ID"
// @Success 200 {object} player.GroupView "同步组"
// @Failure 404 {object} map[string]string "组不存在"
// @Security BearerAuth
// @Router /groups/{id} [get]
func (h *PlaybackHandler) GetGroup(w http.ResponseWriter, r *http.Request) {
	g, err := h.mgr.GetGroup(chi.URLParam(r, "id"))
	if err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, g)
}

// CreateGroup 新建组
// @Summary 新建同步组
// @Description 用给定设备创建同步组。设备会从原组自动摘出（一台设备同时只能属于一个组）。master_id 必须在 member_ids 中，缺省取第一个。
// @Tags 播放控制
// @Accept json
// @Produce json
// @Param request body createGroupRequest true "组信息"
// @Success 201 {object} player.GroupView "新建的组"
// @Failure 400 {object} map[string]string "参数错误"
// @Security BearerAuth
// @Router /groups [post]
func (h *PlaybackHandler) CreateGroup(w http.ResponseWriter, r *http.Request) {
	var req createGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	g, err := h.mgr.CreateGroup(r.Context(), req.Name, req.MemberIDs, req.MasterID)
	if err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, g)
}

type createGroupRequest struct {
	Name      string   `json:"name"`
	MemberIDs []string `json:"member_ids"`
	MasterID  string   `json:"master_id"`
}

// AddMember 加入成员
// @Summary 向同步组加入设备
// @Description 播放中加入设备，从当前时间点接入（不回放已播部分）。
// @Tags 播放控制
// @Accept json
// @Produce json
// @Param id path string true "组 ID"
// @Param request body memberRequest true "设备"
// @Success 200 {object} player.GroupView "更新后的组"
// @Failure 404 {object} map[string]string "组或设备不存在"
// @Failure 409 {object} map[string]string "设备已在组内"
// @Security BearerAuth
// @Router /groups/{id}/members [post]
func (h *PlaybackHandler) AddMember(w http.ResponseWriter, r *http.Request) {
	var req memberRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	g, err := h.mgr.AddMember(r.Context(), chi.URLParam(r, "id"), req.PlayerID)
	if err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, g)
}

type memberRequest struct {
	PlayerID string `json:"player_id"`
}

type memberUpdateRequest struct {
	DelayMs      int `json:"delay_ms"`
	VolumeOffset int `json:"volume_offset"`
}

// UpdateMember 调整成员的延迟补偿与音量偏移
// @Summary 调整组成员的延迟补偿与音量偏移
// @Description delay_ms 为"该设备比主控晚出声多少毫秒"（-2000~2000，正值=更晚），音量偏移 -100~100。改动立即生效：延迟通过重新挂载该路目标写入起播偏移，不需要重新起播。
// @Tags 播放控制
// @Accept json
// @Produce json
// @Param id path string true "组 ID"
// @Param player_id path string true "设备 ID"
// @Param request body memberUpdateRequest true "延迟与音量偏移"
// @Success 200 {object} player.GroupView "更新后的组"
// @Failure 400 {object} map[string]string "请求格式错误"
// @Failure 404 {object} map[string]string "组或设备不存在"
// @Security BearerAuth
// @Router /groups/{id}/members/{player_id} [patch]
func (h *PlaybackHandler) UpdateMember(w http.ResponseWriter, r *http.Request) {
	var req memberUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	g, err := h.mgr.UpdateMember(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "player_id"), req.DelayMs, req.VolumeOffset)
	if err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, g)
}

type groupVolumeRequest struct {
	Volume int  `json:"volume"`
	Muted  bool `json:"muted"`
}

// GroupVolume 读组音量
// @Summary 读取组音量
// @Description 返回基准音量与每个成员的实际音量（实际值 = clamp(基准 + 成员音量偏移)）。基准由主控反推。
// @Tags 播放控制
// @Produce json
// @Param id path string true "组 ID"
// @Success 200 {object} player.GroupVolume "组音量"
// @Failure 404 {object} map[string]string "组不存在"
// @Security BearerAuth
// @Router /groups/{id}/volume [get]
func (h *PlaybackHandler) GroupVolume(w http.ResponseWriter, r *http.Request) {
	v, err := h.mgr.GroupVolumeOf(chi.URLParam(r, "id"))
	if err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, v)
}

// SetGroupVolume 下发组音量
// @Summary 同步下发组音量
// @Description 一次性给组内每台设备下发 clamp(volume + volume_offset)。部分设备失败不影响其他设备（已成功的成员不回滚），失败项在 members[].error 中返回。
// @Tags 播放控制
// @Accept json
// @Produce json
// @Param id path string true "组 ID"
// @Param request body groupVolumeRequest true "音量 0-100 与静音"
// @Success 200 {object} player.GroupVolume "下发结果（含每成员实际值）"
// @Failure 400 {object} map[string]string "请求格式错误"
// @Failure 404 {object} map[string]string "组不存在"
// @Security BearerAuth
// @Router /groups/{id}/volume [post]
func (h *PlaybackHandler) SetGroupVolume(w http.ResponseWriter, r *http.Request) {
	var req groupVolumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	v, err := h.mgr.SetGroupVolume(r.Context(), chi.URLParam(r, "id"), req.Volume, req.Muted)
	if err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, v)
}

// RemoveMember 移出成员
// @Summary 从同步组移出设备
// @Description mode=continue（默认）时该设备继续播放自己的内容；mode=stop 时立即停止出声。移出后设备回到自己的单机组。
// @Tags 播放控制
// @Produce json
// @Param id path string true "组 ID"
// @Param player_id path string true "设备 ID"
// @Param mode query string false "continue 或 stop" default(continue)
// @Success 200 {object} map[string]string "操作结果"
// @Failure 404 {object} map[string]string "组或设备不存在"
// @Security BearerAuth
// @Router /groups/{id}/members/{player_id} [delete]
func (h *PlaybackHandler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "continue"
	}
	if mode != "continue" && mode != "stop" {
		respondError(w, http.StatusBadRequest, "mode 只能是 continue 或 stop", nil)
		return
	}
	if err := h.mgr.RemoveMember(r.Context(), chi.URLParam(r, "id"), chi.URLParam(r, "player_id"), mode); err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Transfer 转移播放
// @Summary 转移播放到另一个组
// @Description 把当前播放（队列 + 位置）从 from_group_id 转到目标组。only_player_id 非空时表示"只在这台音箱上播"：把它从原组拆出来单独成组。源组在派对中时返回 409。
// @Tags 播放控制
// @Accept json
// @Produce json
// @Param id path string true "目标组 ID"
// @Param request body transferRequest true "转移参数"
// @Success 200 {object} map[string]string "操作结果"
// @Failure 409 {object} map[string]string "组在派对中"
// @Security BearerAuth
// @Router /groups/{id}/transfer [post]
func (h *PlaybackHandler) Transfer(w http.ResponseWriter, r *http.Request) {
	var req transferRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	if err := h.mgr.Transfer(r.Context(), req.FromGroupID, chi.URLParam(r, "id"), req.OnlyPlayerID); err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type transferRequest struct {
	FromGroupID  string `json:"from_group_id"`
	OnlyPlayerID string `json:"only_player_id"`
}

// GetQueue 队列快照
// @Summary 获取组队列
// @Description 返回组的队列（歌曲 ID 规范顺序）、当前下标、循环与随机模式。
// @Tags 播放控制
// @Produce json
// @Param id path string true "组 ID"
// @Success 200 {object} player.Status "播放状态与队列"
// @Failure 404 {object} map[string]string "组不存在"
// @Security BearerAuth
// @Router /groups/{id}/queue [get]
func (h *PlaybackHandler) GetQueue(w http.ResponseWriter, r *http.Request) {
	e, err := h.mgr.Engine(chi.URLParam(r, "id"))
	if err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, e.Status())
}

// AddToQueue 追加歌曲
// @Summary 向组队列追加歌曲
// @Description 追加一批歌曲到队列尾部。队列为空时自动从第一首开始（不自动播放）。
// @Tags 播放控制
// @Accept json
// @Produce json
// @Param id path string true "组 ID"
// @Param request body queueAddRequest true "歌曲"
// @Success 200 {object} player.Status "更新后的状态"
// @Failure 404 {object} map[string]string "组不存在"
// @Security BearerAuth
// @Router /groups/{id}/queue [post]
func (h *PlaybackHandler) AddToQueue(w http.ResponseWriter, r *http.Request) {
	var req queueAddRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	e, err := h.mgr.Engine(chi.URLParam(r, "id"))
	if err != nil {
		playbackFail(w, err)
		return
	}
	e.Enqueue(req.SongIDs...)
	respondJSON(w, http.StatusOK, e.Status())
}

type queueAddRequest struct {
	SongIDs []int64 `json:"song_ids"`
}

// ReorderQueue 重排队列
// @Summary 重排队列
// @Description move 语义：把 from 位置的歌曲移到 to 位置。remove_index 非空时先删除该位置（删除与重排互斥，二选一）。
// @Tags 播放控制
// @Accept json
// @Produce json
// @Param id path string true "组 ID"
// @Param request body queueReorderRequest true "重排参数"
// @Success 200 {object} player.Status "更新后的状态"
// @Failure 400 {object} map[string]string "下标越界"
// @Security BearerAuth
// @Router /groups/{id}/queue/reorder [put]
func (h *PlaybackHandler) ReorderQueue(w http.ResponseWriter, r *http.Request) {
	var req queueReorderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	e, err := h.mgr.Engine(chi.URLParam(r, "id"))
	if err != nil {
		playbackFail(w, err)
		return
	}
	if req.RemoveIndex != nil {
		if err := e.RemoveAt(*req.RemoveIndex); err != nil {
			playbackFail(w, err)
			return
		}
	} else if req.From != nil && req.To != nil {
		if err := e.Move(*req.From, *req.To); err != nil {
			playbackFail(w, err)
			return
		}
	} else {
		respondError(w, http.StatusBadRequest, "需要 remove_index 或 (from, to)", nil)
		return
	}
	respondJSON(w, http.StatusOK, e.Status())
}

type queueReorderRequest struct {
	From        *int `json:"from"`
	To          *int `json:"to"`
	RemoveIndex *int `json:"remove_index"`
}

// Transport 传输控制
// @Summary 播放控制
// @Description action: play / pause / stop / next / prev / seek（seek 需带 position 秒）。repeat 与 shuffle 也走这里：repeat=off|all|one，shuffle=true|false。
// @Tags 播放控制
// @Accept json
// @Produce json
// @Param id path string true "组 ID"
// @Param request body transportRequest true "动作"
// @Success 200 {object} player.Status "更新后的状态"
// @Failure 400 {object} map[string]string "参数错误"
// @Failure 503 {object} map[string]string "解码槽位已满"
// @Security BearerAuth
// @Router /groups/{id}/transport [post]
func (h *PlaybackHandler) Transport(w http.ResponseWriter, r *http.Request) {
	var req transportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	e, err := h.mgr.Engine(chi.URLParam(r, "id"))
	if err != nil {
		playbackFail(w, err)
		return
	}
	ctx := r.Context()
	switch req.Action {
	case "play":
		err = e.Play(ctx)
	case "pause":
		err = e.Pause()
	case "stop":
		e.Stop()
	case "next":
		err = e.Next(ctx, false)
	case "prev":
		err = e.Prev(ctx)
	case "seek":
		if req.Position == nil {
			respondError(w, http.StatusBadRequest, "seek 需要 position（秒）", nil)
			return
		}
		err = e.Seek(ctx, time.Duration(*req.Position*float64(time.Second)))
	case "repeat":
		m, verr := player.ValidateRepeat(req.Repeat)
		if verr != nil {
			respondError(w, http.StatusBadRequest, verr.Error(), nil)
			return
		}
		e.SetRepeat(m)
	case "shuffle":
		if req.Shuffle == nil {
			respondError(w, http.StatusBadRequest, "shuffle 需要布尔值", nil)
			return
		}
		e.SetShuffle(*req.Shuffle)
	default:
		respondError(w, http.StatusBadRequest, "不支持的 action", nil)
		return
	}
	if err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, e.Status())
}

type transportRequest struct {
	Action   string   `json:"action"`
	Position *float64 `json:"position"`
	Repeat   string   `json:"repeat"`
	Shuffle  *bool    `json:"shuffle"`
}

// Playback 全量对齐
// @Summary 获取全部播放状态
// @Description 返回所有组的播放状态。客户端断线重连后必须调一次做全量对齐，不能只依赖增量事件。
// @Tags 播放控制
// @Produce json
// @Success 200 {array} player.GroupView "全部组状态"
// @Security BearerAuth
// @Router /playback [get]
func (h *PlaybackHandler) Playback(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, h.mgr.ListGroups())
}

// EventsTicket 签发 WS ticket
// @Summary 获取 WebSocket 一次性 ticket
// @Description 浏览器无法给 WS 带 Authorization 头，token 进 URL 又会落访问日志。先取一个 30 秒有效、绑定当前用户、用后即焚的 ticket，再连 /playback/events?ticket=...。
// @Tags 播放控制
// @Produce json
// @Success 200 {object} ticketResponse "ticket"
// @Security BearerAuth
// @Router /playback/events/ticket [post]
func (h *PlaybackHandler) EventsTicket(w http.ResponseWriter, r *http.Request) {
	user := "anonymous"
	if clientID := appmiddleware.ClientIDFromContext(r.Context()); clientID != "" {
		user = clientID
	}
	token, exp, err := h.hub.Tickets().Issue(user)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "签发 ticket 失败", err)
		return
	}
	respondJSON(w, http.StatusOK, ticketResponse{Ticket: token, ExpiresAt: exp.UnixMilli()})
}

type ticketResponse struct {
	Ticket    string `json:"ticket"`
	ExpiresAt int64  `json:"expires_at"`
}

// Events WS 升级
// @Summary 播放事件 WebSocket
// @Description 升级为 WebSocket 连接推送播放事件（state / member_changed / party_changed / group_drift / error 等）。鉴权用 POST /playback/events/ticket 换到的一次性 ticket，放在 query 参数 ticket 上；ticket 30 秒有效且用后即焚。客户端需按 seq 去重，断线重连后调 /playback 全量对齐。
// @Tags 播放控制
// @Produce json
// @Param ticket query string true "一次性 ticket"
// @Success 101 {string} string "Switching Protocols"
// @Failure 401 {string} string "ticket 无效"
// @Router /playback/events [get]
func (h *PlaybackHandler) Events(w http.ResponseWriter, r *http.Request) {
	h.hub.ServeHTTP(w, r)
}

// PartyStatus 派对状态
// @Summary 获取派对状态
// @Description 返回派对成员组与各组相对主控组的偏移（毫秒）。|offset| 超过 party_drift_tolerance_ms 时 drifting=true，前端应显示"未对齐"而不是反复弹提示。
// @Tags 播放控制
// @Produce json
// @Param id path string true "派对 ID"
// @Success 200 {object} player.PartyStatus "派对状态"
// @Failure 404 {object} map[string]string "派对不存在"
// @Security BearerAuth
// @Router /playback/party/{id} [get]
func (h *PlaybackHandler) PartyStatus(w http.ResponseWriter, r *http.Request) {
	st, err := h.mgr.Status(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, st)
}

// CreateParty 创建派对
// @Summary 创建派对
// @Description 把多个组放进同一个派对，同播同一份内容。每个成员组各自解码，因此每组各占一个解码槽位。party_enabled=false 时返回 403。
// @Tags 播放控制
// @Accept json
// @Produce json
// @Param request body createPartyRequest true "派对信息"
// @Success 201 {object} player.Party "派对"
// @Failure 403 {object} map[string]string "派对模式已关闭"
// @Failure 409 {object} map[string]string "组已在别的派对中"
// @Security BearerAuth
// @Router /playback/party [post]
func (h *PlaybackHandler) CreateParty(w http.ResponseWriter, r *http.Request) {
	var req createPartyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	p, err := h.mgr.CreateParty(r.Context(), req.Name, req.GroupIDs)
	if err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, p)
}

type createPartyRequest struct {
	Name     string   `json:"name"`
	GroupIDs []string `json:"group_ids"`
}

// PartyGroups 派对成员组操作
// @Summary 派对成员组加入 / 退出 / 结束
// @Description action=join 加入组 {group_id}；action=leave 退出组 {group_id, mode}；action=end 结束整个派对。
// @Tags 播放控制
// @Accept json
// @Produce json
// @Param id path string true "派对 ID"
// @Param request body partyGroupRequest true "操作"
// @Success 200 {object} map[string]string "操作结果"
// @Failure 404 {object} map[string]string "派对或组不存在"
// @Security BearerAuth
// @Router /playback/party/{id}/groups [post]
func (h *PlaybackHandler) PartyGroups(w http.ResponseWriter, r *http.Request) {
	var req partyGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	pid := chi.URLParam(r, "id")
	var err error
	switch req.Action {
	case "join":
		_, err = h.mgr.JoinParty(r.Context(), pid, req.GroupID)
	case "leave":
		err = h.mgr.LeaveParty(r.Context(), pid, req.GroupID, req.Mode)
	case "end":
		err = h.mgr.EndParty(r.Context(), pid)
	default:
		respondError(w, http.StatusBadRequest, "不支持的 action", nil)
		return
	}
	if err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type partyGroupRequest struct {
	Action  string `json:"action"`
	GroupID string `json:"group_id"`
	Mode    string `json:"mode"`
}

// PartyTransport 派对传输控制
// @Summary 派对播放控制
// @Description action: play / pause / stop，作用于派对内全部组。
// @Tags 播放控制
// @Accept json
// @Produce json
// @Param id path string true "派对 ID"
// @Param request body partyTransportRequest true "动作"
// @Success 200 {object} map[string]string "操作结果"
// @Security BearerAuth
// @Router /playback/party/{id}/transport [post]
func (h *PlaybackHandler) PartyTransport(w http.ResponseWriter, r *http.Request) {
	var req partyTransportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	if err := h.mgr.PartyTransport(r.Context(), chi.URLParam(r, "id"), req.Action); err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type partyTransportRequest struct {
	Action string `json:"action"`
}

// DLNANotify 接收 DLNA 设备的 GENA 事件。
//
// 公开端点：设备不会带 JWT。安全性靠来源 IP 校验——只接受来自该设备
// 自身地址的 NOTIFY（见 Manager.HandleSinkNotify），body 限 64KB。
//
// @Summary 接收 DLNA 设备事件回调
// @Description DLNA 渲染器通过 GENA NOTIFY 推送 TransportState/位置变化。设备无法携带 JWT，因此该端点公开，仅接受来源 IP 与设备地址一致的请求；其他来源返回 403。body 为 UPnP LastChange 属性集，超过 64KB 截断。
// @Tags 播放控制
// @Accept xml
// @Produce json
// @Param id path string true "设备 ID"
// @Success 200 {object} map[string]string "已接收"
// @Failure 403 {object} map[string]string "来源 IP 与设备地址不一致"
// @Failure 404 {object} map[string]string "设备不存在"
// @Router /playback/dlna/notify/{id} [post]
func (h *PlaybackHandler) DLNANotify(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		respondError(w, http.StatusBadRequest, "读取事件失败", err)
		return
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if err := h.mgr.HandleSinkNotify(host, chi.URLParam(r, "id"), body); err != nil {
		playbackFail(w, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}
