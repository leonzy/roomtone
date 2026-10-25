package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// 本文件是设备 / 组的删除与主控切换端点（B8-04）。
//
// 单独成文件是为了让 playback.go 只留"日常操作"，删除这类不可逆动作
// 集中一处，便于审查它们都先停播放再摘关系。

// DeletePlayer 删除设备
// @Summary 删除播放设备
// @Description 永久删除设备：停掉它的播放、从所有组中摘出、删除持久化记录。若它是所在组的唯一成员，该组一并删除。删掉最后一台设备后系统处于"没有任何音箱"的合法状态，列表返回空数组而不是报错。不可撤销。
// @Tags 播放控制
// @Produce json
// @Param id path string true "设备 ID"
// @Success 200 {object} map[string]string "删除结果"
// @Failure 404 {object} map[string]string "设备不存在"
// @Security BearerAuth
// @Router /players/{id} [delete]
func (h *PlaybackHandler) DeletePlayer(w http.ResponseWriter, r *http.Request) {
	if err := h.mgr.DeletePlayer(r.Context(), chi.URLParam(r, "id")); err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// DeleteGroup 删除组
// @Summary 删除同步组
// @Description 删除组：先停止播放并释放解码槽位，成员各自回到自己的单机组（设备本身保留）。派对中的组不能删除，返回 409，需先退出派对。
// @Tags 播放控制
// @Produce json
// @Param id path string true "组 ID"
// @Success 200 {object} map[string]string "删除结果"
// @Failure 404 {object} map[string]string "组不存在"
// @Failure 409 {object} map[string]string "组在派对中"
// @Security BearerAuth
// @Router /groups/{id} [delete]
func (h *PlaybackHandler) DeleteGroup(w http.ResponseWriter, r *http.Request) {
	if err := h.mgr.DeleteGroup(r.Context(), chi.URLParam(r, "id")); err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type masterRequest struct {
	PlayerID string `json:"player_id"`
}

// SetMaster 切换主控
// @Summary 切换同步组的主控设备
// @Description 把组的主控换成组内另一台设备。主控决定组音量的基准值，不参与时钟同步，因此播放中切换不会断音、不需要重新握手。设备不在该组返回 404。
// @Tags 播放控制
// @Accept json
// @Produce json
// @Param id path string true "组 ID"
// @Param request body masterRequest true "新的主控设备"
// @Success 200 {object} player.GroupView "更新后的组"
// @Failure 400 {object} map[string]string "请求格式错误"
// @Failure 404 {object} map[string]string "组不存在或设备不在组内"
// @Security BearerAuth
// @Router /groups/{id}/master [post]
func (h *PlaybackHandler) SetMaster(w http.ResponseWriter, r *http.Request) {
	var req masterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	if req.PlayerID == "" {
		respondError(w, http.StatusBadRequest, "player_id 不能为空", nil)
		return
	}
	g, err := h.mgr.SetMaster(r.Context(), chi.URLParam(r, "id"), req.PlayerID)
	if err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, g)
}

// Metrics 播放指标
// @Summary 播放子系统运行指标
// @Description 返回一次性快照：解码槽位占用（decode_slots_used/max）、欠载补静音帧数（player_underruns_total）、分发队列丢帧（player_sink_dropped_frames_total）、偏差超限重挂次数（player_resyncs_total）、事件丢弃数（player_events_dropped_total）、各在播 owner（组/派对）的最近偏差与缓冲状态。用于排查"声音卡"是解码跟不上还是网络慢；不是 Prometheus 端点。
// @Tags 播放控制
// @Produce json
// @Success 200 {object} player.MetricsSnapshot "指标快照"
// @Security BearerAuth
// @Router /playback/metrics [get]
func (h *PlaybackHandler) Metrics(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, h.mgr.MetricsSnapshot())
}

type powerRequest struct {
	On bool `json:"on"`
}

// SetPower 待机 / 唤醒设备
// @Summary 待机或唤醒播放设备
// @Description on=false 让设备待机：DLNA 发 Stop 并清空当前 URI，RAOP 停发 RTP；待机设备立刻从组内分发与同步校正中摘除，不会收到音频。on=true 唤醒并重新挂回组，从当前进度接入。注意 RTSP / DLNA 都没有通用的硬件上电指令，接口返回的 standby 只表示"服务端不再向它推流"，不代表设备真的断电待机。
// @Tags 播放控制
// @Accept json
// @Produce json
// @Param id path string true "设备 ID"
// @Param request body powerRequest true "on=true 唤醒，false 待机"
// @Success 200 {object} player.Player "更新后的设备"
// @Failure 400 {object} map[string]string "请求格式错误"
// @Failure 404 {object} map[string]string "设备不存在"
// @Security BearerAuth
// @Router /players/{id}/power [post]
func (h *PlaybackHandler) SetPower(w http.ResponseWriter, r *http.Request) {
	var req powerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	p, err := h.mgr.SetPower(r.Context(), chi.URLParam(r, "id"), req.On)
	if err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, p)
}
