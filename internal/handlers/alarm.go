package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"songloft/internal/database"
)

// AlarmHandler 服务端闹钟的 CRUD 与手动触发。
//
// 调度本身在 app.alarmRunner 里（后台循环），handler 只管数据与"试听"。
// Trigger 由 app 注入，避免 handler 直接依赖播放管理器。
type AlarmHandler struct {
	repo    *database.AlarmRepository
	trigger func(ctx context.Context, id string) error
}

// NewAlarmHandler 创建闹钟 handler。trigger 可以为 nil（此时 /test 返回 503）。
func NewAlarmHandler(repo *database.AlarmRepository, trigger func(ctx context.Context, id string) error) *AlarmHandler {
	return &AlarmHandler{repo: repo, trigger: trigger}
}

// AlarmRequest 闹钟创建 / 更新请求体。
type AlarmRequest struct {
	Name string `json:"name" example:"起床"`
	// TargetID 组 id 或设备（player）id，设备会解析成它所在的组。
	TargetID string `json:"target_id" example:"group-1"`
	// TimeOfDay 本地时刻 "HH:MM"。
	TimeOfDay string `json:"time_of_day" example:"07:30"`
	// DaysMask 星期掩码，bit0=周一 … bit6=周日，127=每天，0=只在今天触发一次。
	DaysMask int `json:"days_mask" example:"127"`
	// PlaylistID 起播的歌单；0 表示只调整音量、不起播。
	PlaylistID int64 `json:"playlist_id" example:"3"`
	// Volume 目标音量 0-100。
	Volume int `json:"volume" example:"40"`
	// FadeSeconds 渐强时长（秒），0 表示直接设到目标音量。
	FadeSeconds int  `json:"fade_seconds" example:"30"`
	Enabled     bool `json:"enabled" example:"true"`
}

// AlarmResponse 闹钟视图。
type AlarmResponse struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	TargetID    string `json:"target_id"`
	TimeOfDay   string `json:"time_of_day"`
	DaysMask    int    `json:"days_mask"`
	PlaylistID  int64  `json:"playlist_id"`
	Volume      int    `json:"volume"`
	FadeSeconds int    `json:"fade_seconds"`
	Enabled     bool   `json:"enabled"`
	LastFiredAt string `json:"last_fired_at"`
}

func alarmView(a *database.AlarmRow) AlarmResponse {
	return AlarmResponse{
		ID: a.ID, Name: a.Name, TargetID: a.TargetID, TimeOfDay: a.TimeOfDay,
		DaysMask: a.DaysMask, PlaylistID: a.PlaylistID, Volume: a.Volume,
		FadeSeconds: a.FadeSeconds, Enabled: a.Enabled, LastFiredAt: a.LastFiredAt,
	}
}

// validateAlarm 校验请求体并归一化。
func validateAlarm(req *AlarmRequest) string {
	req.Name = strings.TrimSpace(req.Name)
	req.TimeOfDay = strings.TrimSpace(req.TimeOfDay)
	req.TargetID = strings.TrimSpace(req.TargetID)
	if req.TargetID == "" {
		return "target_id 不能为空（组 id 或设备 id）"
	}
	if _, _, ok := parseHHMMValue(req.TimeOfDay); !ok {
		return "time_of_day 必须是 HH:MM（00:00-23:59）"
	}
	if req.DaysMask < 0 || req.DaysMask > 127 {
		return "days_mask 必须在 0-127 之间（bit0=周一 … bit6=周日，0=只触发一次）"
	}
	if req.Volume < 0 || req.Volume > 100 {
		return "volume 必须在 0-100 之间"
	}
	if req.FadeSeconds < 0 || req.FadeSeconds > 300 {
		return "fade_seconds 必须在 0-300 之间"
	}
	if req.PlaylistID < 0 {
		return "playlist_id 不能为负"
	}
	return ""
}

// parseHHMMValue 只做格式校验，调度器里有同名逻辑但分属两层（handler 不 import app）。
func parseHHMMValue(s string) (int, int, bool) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	h := atoiClamp(parts[0], 23)
	m := atoiClamp(parts[1], 59)
	if h < 0 || m < 0 {
		return 0, 0, false
	}
	return h, m, true
}

func atoiClamp(s string, max int) int {
	if s == "" {
		return -1
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return -1
		}
		n = n*10 + int(c-'0')
	}
	if n > max {
		return -1
	}
	return n
}

func newAlarmID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "alarm-" + hex.EncodeToString([]byte{0, 0, 0, 0})
	}
	return "alarm-" + hex.EncodeToString(b)
}

// ListAlarms 列出全部闹钟
// @Summary 列出全部闹钟
// @Description 返回按触发时刻排序的闹钟列表。last_fired_at 记录最近一次触发到的分钟（YYYY-MM-DDTHH:MM），可用于判断今天是否已经响过。
// @Tags 播放历史
// @Produce json
// @Success 200 {array} AlarmResponse "闹钟列表"
// @Security BearerAuth
// @Router /alarms [get]
func (h *AlarmHandler) ListAlarms(w http.ResponseWriter, r *http.Request) {
	rows, err := h.repo.List(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, "读取闹钟失败", err)
		return
	}
	out := make([]AlarmResponse, 0, len(rows))
	for _, a := range rows {
		out = append(out, alarmView(a))
	}
	respondJSON(w, http.StatusOK, out)
}

// CreateAlarm 新建闹钟
// @Summary 新建闹钟
// @Description 到点后在 target_id 对应的组上起播 playlist_id（0 表示只调整音量），volume 为 0-100 的目标音量，fade_seconds 为渐强秒数。time_of_day 用 HH:MM 本地时刻，days_mask 的 bit0=周一 … bit6=周日，0 表示只在今天触发一次。id 由服务端生成。
// @Tags 播放历史
// @Accept json
// @Produce json
// @Param request body AlarmRequest true "闹钟配置"
// @Success 201 {object} AlarmResponse "创建成功"
// @Failure 400 {object} map[string]string "参数非法"
// @Failure 500 {object} map[string]string "写入失败"
// @Security BearerAuth
// @Router /alarms [post]
func (h *AlarmHandler) CreateAlarm(w http.ResponseWriter, r *http.Request) {
	var req AlarmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求体解析失败", err)
		return
	}
	if msg := validateAlarm(&req); msg != "" {
		respondError(w, http.StatusBadRequest, msg, nil)
		return
	}
	row := &database.AlarmRow{
		ID: newAlarmID(), Name: req.Name, TargetID: req.TargetID,
		TimeOfDay: req.TimeOfDay, DaysMask: req.DaysMask, PlaylistID: req.PlaylistID,
		Volume: req.Volume, FadeSeconds: req.FadeSeconds, Enabled: req.Enabled,
	}
	if err := h.repo.Create(r.Context(), row); err != nil {
		respondError(w, http.StatusInternalServerError, "创建闹钟失败", err)
		return
	}
	respondJSON(w, http.StatusCreated, alarmView(row))
}

// UpdateAlarm 更新闹钟
// @Summary 更新闹钟
// @Description 覆盖式更新指定闹钟的全部可编辑字段（name / target_id / time_of_day / days_mask / playlist_id / volume / fade_seconds / enabled）。id 与 last_fired_at 不受影响。
// @Tags 播放历史
// @Accept json
// @Produce json
// @Param id path string true "闹钟 id"
// @Param request body AlarmRequest true "闹钟配置"
// @Success 200 {object} AlarmResponse "更新后的闹钟"
// @Failure 400 {object} map[string]string "参数非法"
// @Failure 404 {object} map[string]string "闹钟不存在"
// @Security BearerAuth
// @Router /alarms/{id} [put]
func (h *AlarmHandler) UpdateAlarm(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req AlarmRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求体解析失败", err)
		return
	}
	if msg := validateAlarm(&req); msg != "" {
		respondError(w, http.StatusBadRequest, msg, nil)
		return
	}
	row := &database.AlarmRow{
		ID: id, Name: req.Name, TargetID: req.TargetID, TimeOfDay: req.TimeOfDay,
		DaysMask: req.DaysMask, PlaylistID: req.PlaylistID, Volume: req.Volume,
		FadeSeconds: req.FadeSeconds, Enabled: req.Enabled,
	}
	if err := h.repo.Update(r.Context(), row); err != nil {
		if errors.Is(err, database.ErrNotFound) {
			respondError(w, http.StatusNotFound, "闹钟不存在", nil)
			return
		}
		respondError(w, http.StatusInternalServerError, "更新闹钟失败", err)
		return
	}
	updated, err := h.repo.Get(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "读取闹钟失败", err)
		return
	}
	respondJSON(w, http.StatusOK, alarmView(updated))
}

// DeleteAlarm 删除闹钟
// @Summary 删除闹钟
// @Description 删除指定闹钟。已触发记录随行一起删除；不影响正在播放的内容。
// @Tags 播放历史
// @Produce json
// @Param id path string true "闹钟 id"
// @Success 204 "删除成功"
// @Failure 404 {object} map[string]string "闹钟不存在"
// @Security BearerAuth
// @Router /alarms/{id} [delete]
func (h *AlarmHandler) DeleteAlarm(w http.ResponseWriter, r *http.Request) {
	if err := h.repo.Delete(r.Context(), chi.URLParam(r, "id")); err != nil {
		if errors.Is(err, database.ErrNotFound) {
			respondError(w, http.StatusNotFound, "闹钟不存在", nil)
			return
		}
		respondError(w, http.StatusInternalServerError, "删除闹钟失败", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// TriggerAlarm 立即触发闹钟
// @Summary 立即触发闹钟
// @Description 不等定时，立刻按闹钟配置起播并渐强音量，便于调试。不写 last_fired_at，因此不影响今天的正常触发。目标组不存在或歌单为空时返回 500 并说明原因。
// @Tags 播放历史
// @Produce json
// @Param id path string true "闹钟 id"
// @Success 200 {object} map[string]string "已触发"
// @Failure 404 {object} map[string]string "闹钟不存在"
// @Failure 500 {object} map[string]string "触发失败"
// @Failure 503 {object} map[string]string "播放子系统不可用"
// @Security BearerAuth
// @Router /alarms/{id}/test [post]
func (h *AlarmHandler) TriggerAlarm(w http.ResponseWriter, r *http.Request) {
	if h.trigger == nil {
		respondError(w, http.StatusServiceUnavailable, "播放子系统不可用", nil)
		return
	}
	id := chi.URLParam(r, "id")
	if _, err := h.repo.Get(r.Context(), id); err != nil {
		if errors.Is(err, database.ErrNotFound) {
			respondError(w, http.StatusNotFound, "闹钟不存在", nil)
			return
		}
		respondError(w, http.StatusInternalServerError, "读取闹钟失败", err)
		return
	}
	if err := h.trigger(r.Context(), id); err != nil {
		respondError(w, http.StatusInternalServerError, "触发失败", err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "triggered"})
}
