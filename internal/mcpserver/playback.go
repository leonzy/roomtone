package mcpserver

import (
	"context"
	"sort"
	"time"

	"songloft/internal/player"
)

func (s *Server) zoneID(id string) (string, error) {
	if id != "" {
		return id, nil
	}
	groups := s.deps.Players.ListGroups()
	online := make([]player.GroupView, 0, len(groups))
	for _, group := range groups {
		for _, member := range group.MemberCaps {
			if member.Online {
				online = append(online, group)
				break
			}
		}
	}
	if len(online) == 1 {
		return online[0].ID, nil
	}
	if len(online) == 0 {
		return "", &ToolError{Code: "ZONE_NOT_FOUND", Message: "没有可用的在线播放区域"}
	}
	return "", &ToolError{Code: "ZONE_REQUIRED", Message: "存在多个在线区域，请指定 zone_id"}
}

func (s *Server) engine(zoneID string) (*player.Engine, error) {
	id, err := s.zoneID(zoneID)
	if err != nil {
		return nil, err
	}
	e, err := s.deps.Players.Engine(id)
	return e, err
}

func (s *Server) validateSongs(ctx context.Context, ids []int64) error {
	if len(ids) == 0 || len(ids) > 500 {
		return invalidInput("song_ids 数量必须为 1-500")
	}
	for _, id := range ids {
		if id <= 0 {
			return invalidInput("song_id 必须为正整数")
		}
		if _, err := s.deps.Songs.GetByID(ctx, id); err != nil {
			return &ToolError{Code: "SONG_NOT_FOUND", Message: "歌曲不存在或不可用"}
		}
	}
	return nil
}

func (s *Server) registerPlaybackTools() {
	registerTool(s, ScopeRead, "get_playback", "获取所有或指定区域的播放状态、当前 song_id、进度、队列、循环、随机模式、成员与同步等级。", true,
		func(_ context.Context, in struct {
			ZoneID     string `json:"zone_id,omitempty"`
			QueueLimit int    `json:"queue_limit,omitempty"`
		}) (any, error) {
			groups := []player.GroupView{}
			if in.ZoneID != "" {
				group, err := s.deps.Players.GetGroup(in.ZoneID)
				if err != nil {
					return nil, err
				}
				groups = append(groups, group)
			} else {
				groups = s.deps.Players.ListGroups()
			}
			limit := pageLimit(in.QueueLimit, 30, 100)
			out := make([]ZoneView, 0, len(groups))
			for _, group := range groups {
				view := zoneView(group)
				if len(view.Status.Queue.Items) > limit {
					view.Status.Queue.Items = append([]int64(nil), view.Status.Queue.Items[:limit]...)
					view.Status.Queue.Started = nil
				}
				out = append(out, view)
			}
			return map[string]any{"zones": out, "queue_limit": limit}, nil
		})

	registerTool(s, ScopePlayback, "play_songs", "在指定区域播放歌曲。mode=now 替换队列并起播，next 插到当前曲之后，end 追加到队尾；未指定区域时仅允许存在一个在线区域。", false,
		func(ctx context.Context, in struct {
			ZoneID     string  `json:"zone_id,omitempty"`
			SongIDs    []int64 `json:"song_ids"`
			Mode       string  `json:"mode,omitempty"`
			StartIndex int     `json:"start_index,omitempty"`
		}) (any, error) {
			if err := s.validateSongs(ctx, in.SongIDs); err != nil {
				return nil, err
			}
			e, err := s.engine(in.ZoneID)
			if err != nil {
				return nil, err
			}
			mode := in.Mode
			if mode == "" {
				mode = "now"
			}
			switch mode {
			case "now":
				if in.StartIndex < 0 || in.StartIndex >= len(in.SongIDs) {
					return nil, invalidInput("start_index 超出 song_ids 范围")
				}
				err = e.Load(ctx, in.SongIDs, in.StartIndex, 0)
			case "next":
				e.InsertNextMany(ctx, in.SongIDs...)
			case "end":
				e.Enqueue(in.SongIDs...)
			default:
				return nil, invalidInput("mode 只支持 now / next / end")
			}
			if err != nil {
				return nil, err
			}
			return mutation(e.Status(), map[string]any{"mode": mode, "song_ids": in.SongIDs}), nil
		})

	registerTool(s, ScopePlayback, "control_playback", "控制区域播放：action 支持 play/pause/stop/next/prev/seek；也可单独设置 repeat=off/all/one 和 shuffle。", false,
		func(ctx context.Context, in struct {
			ZoneID          string   `json:"zone_id,omitempty"`
			Action          string   `json:"action,omitempty"`
			PositionSeconds *float64 `json:"position_seconds,omitempty"`
			Repeat          string   `json:"repeat,omitempty"`
			Shuffle         *bool    `json:"shuffle,omitempty"`
		}) (any, error) {
			e, err := s.engine(in.ZoneID)
			if err != nil {
				return nil, err
			}
			if in.Action == "" {
				if in.Repeat == "" && in.Shuffle == nil {
					return nil, invalidInput("需要 action、repeat 或 shuffle")
				}
				if in.Repeat != "" {
					repeat, err := player.ValidateRepeat(in.Repeat)
					if err != nil {
						return nil, err
					}
					e.SetRepeat(repeat)
				}
				if in.Shuffle != nil {
					e.SetShuffle(*in.Shuffle)
				}
				return mutation(e.Status(), nil), nil
			}
			switch in.Action {
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
				if in.PositionSeconds == nil || *in.PositionSeconds < 0 {
					return nil, invalidInput("seek 需要非负 position_seconds")
				}
				err = e.Seek(ctx, time.Duration(*in.PositionSeconds*float64(time.Second)))
			default:
				return nil, invalidInput("不支持的 action")
			}
			if err != nil {
				return nil, err
			}
			return mutation(e.Status(), map[string]any{"action": in.Action}), nil
		})

	registerTool(s, ScopePlayback, "remove_queue_items", "按队列下标删除一个或多个条目；下标来自 get_playback 返回的规范顺序。", false,
		func(_ context.Context, in struct {
			ZoneID  string `json:"zone_id,omitempty"`
			Indices []int  `json:"indices"`
		}) (any, error) {
			if len(in.Indices) == 0 {
				return nil, invalidInput("indices 不能为空")
			}
			e, err := s.engine(in.ZoneID)
			if err != nil {
				return nil, err
			}
			indices := append([]int(nil), in.Indices...)
			sort.Sort(sort.Reverse(sort.IntSlice(indices)))
			previous := -1
			for _, index := range indices {
				if index < 0 || index == previous {
					return nil, invalidInput("indices 不能为负数或包含重复值")
				}
				if err := e.RemoveAt(index); err != nil {
					return nil, err
				}
				previous = index
			}
			return mutation(e.Status(), map[string]any{"removed_indices": in.Indices}), nil
		})

	registerTool(s, ScopePlayback, "reorder_queue", "把区域队列中 from 下标的条目移动到 to 下标。", false,
		func(_ context.Context, in struct {
			ZoneID string `json:"zone_id,omitempty"`
			From   int    `json:"from"`
			To     int    `json:"to"`
		}) (any, error) {
			e, err := s.engine(in.ZoneID)
			if err != nil {
				return nil, err
			}
			if err := e.Move(in.From, in.To); err != nil {
				return nil, err
			}
			return mutation(e.Status(), map[string]any{"from": in.From, "to": in.To}), nil
		})

	registerTool(s, ScopePlayback, "transfer_playback", "将源区域的队列和播放进度转移到目标区域；only_speaker_id 可将一台音箱拆出单独接管。", false,
		func(ctx context.Context, in struct {
			FromZoneID    string `json:"from_zone_id"`
			ToZoneID      string `json:"to_zone_id"`
			OnlySpeakerID string `json:"only_speaker_id,omitempty"`
		}) (any, error) {
			if in.FromZoneID == "" || in.ToZoneID == "" {
				return nil, invalidInput("from_zone_id 和 to_zone_id 不能为空")
			}
			if err := s.deps.Players.Transfer(ctx, in.FromZoneID, in.ToZoneID, in.OnlySpeakerID); err != nil {
				return nil, err
			}
			target, err := s.deps.Players.GetGroup(in.ToZoneID)
			if err == nil {
				return mutation(zoneView(target), nil), nil
			}
			if in.OnlySpeakerID != "" {
				if zoneID := s.deps.Players.GroupIDOf(in.OnlySpeakerID); zoneID != "" {
					target, err = s.deps.Players.GetGroup(zoneID)
					if err == nil {
						return mutation(zoneView(target), nil), nil
					}
				}
			}
			return nil, err
		})
}
