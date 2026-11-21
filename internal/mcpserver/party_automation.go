package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math"
	"strings"
	"time"

	"songloft/internal/database"
	"songloft/internal/player"
)

func (s *Server) registerPartyAutomationTools() {
	registerTool(s, ScopeRead, "get_party", "获取派对成员组及各组相对主控的漂移状态。", true,
		func(ctx context.Context, in struct {
			PartyID string `json:"party_id"`
		}) (any, error) {
			party, err := s.deps.Players.GetParty(in.PartyID)
			if err != nil {
				return nil, err
			}
			status, err := s.deps.Players.Status(ctx, in.PartyID)
			if err != nil {
				return nil, err
			}
			return map[string]any{"party": party, "status": status}, nil
		})

	registerTool(s, ScopeSpeakers, "create_party", "创建多区域派对，共享播放内容但保留各区域独立音量和输出格式；每组占用独立解码槽位。", false,
		func(ctx context.Context, in struct {
			Name    string   `json:"name"`
			ZoneIDs []string `json:"zone_ids"`
		}) (any, error) {
			party, err := s.deps.Players.CreateParty(ctx, in.Name, in.ZoneIDs)
			if err != nil {
				return nil, err
			}
			return mutation(party, nil), nil
		})

	registerTool(s, ScopeSpeakers, "update_party", "管理派对成员：action=join 需要 zone_id；leave 需要 zone_id 和 mode=continue/stop；end 结束整个派对。", false,
		func(ctx context.Context, in struct {
			PartyID string `json:"party_id"`
			Action  string `json:"action"`
			ZoneID  string `json:"zone_id,omitempty"`
			Mode    string `json:"mode,omitempty"`
		}) (any, error) {
			var err error
			switch in.Action {
			case "join":
				_, err = s.deps.Players.JoinParty(ctx, in.PartyID, in.ZoneID)
			case "leave":
				if in.Mode == "" {
					in.Mode = "continue"
				}
				err = s.deps.Players.LeaveParty(ctx, in.PartyID, in.ZoneID, in.Mode)
			case "end":
				err = s.deps.Players.EndParty(ctx, in.PartyID)
			default:
				return nil, invalidInput("action 只支持 join / leave / end")
			}
			if err != nil {
				return nil, err
			}
			return mutation(map[string]any{"party_id": in.PartyID, "action": in.Action}, nil), nil
		})

	registerTool(s, ScopePlayback, "control_party", "对派对全部区域执行 play、pause 或 stop。", false,
		func(ctx context.Context, in struct {
			PartyID string `json:"party_id"`
			Action  string `json:"action"`
		}) (any, error) {
			if err := s.deps.Players.PartyTransport(ctx, in.PartyID, in.Action); err != nil {
				return nil, err
			}
			status, err := s.deps.Players.Status(ctx, in.PartyID)
			if err != nil {
				return nil, err
			}
			return mutation(status, map[string]any{"action": in.Action}), nil
		})

	registerTool(s, ScopeRead, "get_sleep_timer", "查询区域睡眠定时器的模式、剩余秒数或剩余歌曲数。", true,
		func(_ context.Context, in struct {
			ZoneID string `json:"zone_id"`
		}) (any, error) {
			return sleepView(s.deps.Players, in.ZoneID), nil
		})

	registerTool(s, ScopeAutomation, "set_sleep_timer", "设置区域睡眠定时器。minutes=1-720 或 songs=1-1000，二者互斥；显式传 0 取消对应定时器。", false,
		func(_ context.Context, in struct {
			ZoneID  string `json:"zone_id"`
			Minutes *int   `json:"minutes,omitempty"`
			Songs   *int   `json:"songs,omitempty"`
		}) (any, error) {
			if in.Minutes != nil && in.Songs != nil {
				return nil, invalidInput("minutes 与 songs 不能同时设置")
			}
			if in.Minutes == nil && in.Songs == nil {
				return nil, invalidInput("需要 minutes 或 songs")
			}
			var err error
			if in.Songs != nil {
				if *in.Songs < 0 || *in.Songs > 1000 {
					return nil, invalidInput("songs 必须在 0-1000 之间")
				}
				err = s.deps.Players.SetSleepAfterSongs(in.ZoneID, *in.Songs)
			} else {
				if *in.Minutes < 0 || *in.Minutes > 720 {
					return nil, invalidInput("minutes 必须在 0-720 之间")
				}
				err = s.deps.Players.SetSleepTimer(in.ZoneID, time.Duration(*in.Minutes)*time.Minute)
			}
			if err != nil {
				return nil, err
			}
			return mutation(sleepView(s.deps.Players, in.ZoneID), nil), nil
		})

	registerTool(s, ScopeRead, "list_alarms", "列出全部闹钟，包括目标区域、歌单、触发时间、音量和渐强配置。", true,
		func(ctx context.Context, _ struct{}) (any, error) {
			rows, err := s.deps.Alarms.List(ctx)
			if err != nil {
				return nil, err
			}
			out := make([]AlarmView, 0, len(rows))
			for _, row := range rows {
				out = append(out, alarmView(row))
			}
			return map[string]any{"alarms": out, "total": len(out)}, nil
		})

	registerTool(s, ScopeAutomation, "create_alarm", "创建闹钟。time_of_day 为本地 HH:MM；days_mask bit0=周一至 bit6=周日，0 为一次性；playlist_id=0 表示只调音量。", false,
		func(ctx context.Context, in AlarmInput) (any, error) {
			row, err := alarmRow(newAlarmID(), in)
			if err != nil {
				return nil, err
			}
			if err := s.deps.Alarms.Create(ctx, row); err != nil {
				return nil, err
			}
			return mutation(alarmView(row), nil), nil
		})

	registerTool(s, ScopeAutomation, "update_alarm", "按 alarm_id 整体更新闹钟的时间、目标、歌单、音量、渐强和启用状态。", false,
		func(ctx context.Context, in struct {
			AlarmID string `json:"alarm_id"`
			AlarmInput
		}) (any, error) {
			existing, err := s.deps.Alarms.Get(ctx, in.AlarmID)
			if err != nil {
				return nil, err
			}
			row, err := alarmRow(in.AlarmID, in.AlarmInput)
			if err != nil {
				return nil, err
			}
			row.LastFiredAt = existing.LastFiredAt
			if err := s.deps.Alarms.Update(ctx, row); err != nil {
				return nil, err
			}
			return mutation(alarmView(row), nil), nil
		})

	registerTool(s, ScopeAutomation, "delete_alarm", "删除明确 alarm_id 的闹钟，不影响其歌单、歌曲或播放区域。", false,
		func(ctx context.Context, in struct {
			AlarmID string `json:"alarm_id"`
		}) (any, error) {
			if err := s.deps.Alarms.Delete(ctx, in.AlarmID); err != nil {
				return nil, err
			}
			return mutation(map[string]any{"deleted": in.AlarmID}, nil), nil
		})

	registerTool(s, ScopeAutomation, "test_alarm", "立即试听指定闹钟，不写入 last_fired_at；会实际起播并调整目标区域音量。", false,
		func(ctx context.Context, in struct {
			AlarmID string `json:"alarm_id"`
		}) (any, error) {
			if s.deps.TriggerAlarm == nil {
				return nil, &ToolError{Code: "UNSUPPORTED_OPERATION", Message: "闹钟试听不可用"}
			}
			if err := s.deps.TriggerAlarm(ctx, in.AlarmID); err != nil {
				return nil, err
			}
			return mutation(map[string]any{"triggered": in.AlarmID}, nil), nil
		})
}

type AlarmInput struct {
	Name        string `json:"name"`
	TargetID    string `json:"target_id"`
	TimeOfDay   string `json:"time_of_day"`
	DaysMask    int    `json:"days_mask"`
	PlaylistID  int64  `json:"playlist_id,omitempty"`
	Volume      int    `json:"volume"`
	FadeSeconds int    `json:"fade_seconds,omitempty"`
	Enabled     bool   `json:"enabled"`
}

type AlarmView struct {
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

func alarmView(row *database.AlarmRow) AlarmView {
	return AlarmView{ID: row.ID, Name: row.Name, TargetID: row.TargetID, TimeOfDay: row.TimeOfDay,
		DaysMask: row.DaysMask, PlaylistID: row.PlaylistID, Volume: row.Volume,
		FadeSeconds: row.FadeSeconds, Enabled: row.Enabled, LastFiredAt: row.LastFiredAt}
}

func alarmRow(id string, in AlarmInput) (*database.AlarmRow, error) {
	in.Name = strings.TrimSpace(in.Name)
	in.TargetID = strings.TrimSpace(in.TargetID)
	in.TimeOfDay = strings.TrimSpace(in.TimeOfDay)
	if in.Name == "" || in.TargetID == "" {
		return nil, invalidInput("name、target_id 不能为空")
	}
	if _, err := time.Parse("15:04", in.TimeOfDay); err != nil {
		return nil, invalidInput("time_of_day 必须为有效 HH:MM")
	}
	if in.DaysMask < 0 || in.DaysMask > 127 || in.Volume < 0 || in.Volume > 100 || in.FadeSeconds < 0 || in.FadeSeconds > 300 || in.PlaylistID < 0 {
		return nil, invalidInput("闹钟参数超出允许范围")
	}
	return &database.AlarmRow{ID: id, Name: in.Name, TargetID: in.TargetID, TimeOfDay: in.TimeOfDay,
		DaysMask: in.DaysMask, PlaylistID: in.PlaylistID, Volume: in.Volume,
		FadeSeconds: in.FadeSeconds, Enabled: in.Enabled}, nil
}

func newAlarmID() string {
	data := make([]byte, 8)
	if _, err := rand.Read(data); err != nil {
		return "alarm-" + hex.EncodeToString([]byte(time.Now().String()))
	}
	return "alarm-" + hex.EncodeToString(data)
}

func sleepView(mgr *player.Manager, zoneID string) map[string]any {
	if songs, ok := mgr.SleepSongs(zoneID); ok {
		return map[string]any{"zone_id": zoneID, "active": true, "mode": "songs", "remaining_songs": songs, "remaining_seconds": 0}
	}
	deadline, ok := mgr.SleepTimer(zoneID)
	if !ok {
		return map[string]any{"zone_id": zoneID, "active": false, "remaining_songs": 0, "remaining_seconds": 0}
	}
	remaining := math.Max(0, math.Ceil(time.Until(deadline).Seconds()))
	return map[string]any{"zone_id": zoneID, "active": true, "mode": "duration", "remaining_songs": 0, "remaining_seconds": remaining}
}
