package mcpserver

import (
	"context"

	"songloft/internal/player"
)

func zoneResult(group player.GroupView) MutationResult {
	result := mutation(zoneView(group), nil)
	if group.SyncTier == player.SyncAligned.String() {
		result.Warnings = append(result.Warnings, "包含 DLNA 或对齐型设备，区域同步等级为 aligned")
	}
	return result
}

func (s *Server) registerZoneTools() {
	registerTool(s, ScopeRead, "list_zones", "列出所有播放区域、成员、主控、同步等级和播放状态。区域是分组和播放控制使用的 zone_id。", true,
		func(_ context.Context, _ struct{}) (any, error) {
			groups := s.deps.Players.ListGroups()
			out := make([]ZoneView, 0, len(groups))
			for _, group := range groups {
				out = append(out, zoneView(group))
			}
			return map[string]any{"zones": out, "total": len(out)}, nil
		})

	registerTool(s, ScopeSpeakers, "create_zone", "创建同步区域。member_ids 中的设备会从原区域移出；master_id 必须是成员，缺省取第一个。", false,
		func(ctx context.Context, in struct {
			Name      string   `json:"name"`
			MemberIDs []string `json:"member_ids"`
			MasterID  string   `json:"master_id,omitempty"`
		}) (any, error) {
			if len(in.MemberIDs) == 0 {
				return nil, invalidInput("member_ids 不能为空")
			}
			group, err := s.deps.Players.CreateGroup(ctx, in.Name, in.MemberIDs, in.MasterID)
			if err != nil {
				return nil, err
			}
			return zoneResult(group), nil
		})

	registerTool(s, ScopeSpeakers, "update_zone", "修改区域名称或图标。图标支持 speaker/bookshelf/soundbar/headphone/tv/phone/laptop/multiroom。", false,
		func(ctx context.Context, in struct {
			ZoneID string `json:"zone_id"`
			Name   string `json:"name"`
			Icon   string `json:"icon,omitempty"`
		}) (any, error) {
			group, err := s.deps.Players.RenameGroup(ctx, in.ZoneID, in.Name, in.Icon)
			if err != nil {
				return nil, err
			}
			return mutation(zoneView(group), map[string]any{"name": in.Name, "icon": in.Icon}), nil
		})

	registerTool(s, ScopeSpeakers, "delete_zone", "解散明确 zone_id 的区域，成员各自回到单机组；不会删除音箱或歌曲。派对中的区域需先退出派对。", false,
		func(ctx context.Context, in struct {
			ZoneID string `json:"zone_id"`
		}) (any, error) {
			group, err := s.deps.Players.GetGroup(in.ZoneID)
			if err != nil {
				return nil, err
			}
			if err := s.deps.Players.DeleteGroup(ctx, in.ZoneID); err != nil {
				return nil, err
			}
			return mutation(map[string]any{"deleted": in.ZoneID}, map[string]any{"name": group.Name, "member_ids": group.MemberIDs()}), nil
		})

	registerTool(s, ScopeSpeakers, "add_zone_member", "将音箱加入区域；若正在播放，新成员从当前进度接入，并遵守实际同步能力。", false,
		func(ctx context.Context, in struct {
			ZoneID    string `json:"zone_id"`
			SpeakerID string `json:"speaker_id"`
		}) (any, error) {
			group, err := s.deps.Players.AddMember(ctx, in.ZoneID, in.SpeakerID)
			if err != nil {
				return nil, err
			}
			return zoneResult(group), nil
		})

	registerTool(s, ScopeSpeakers, "remove_zone_member", "将音箱移出区域并回到单机组；mode=continue 继续播放，stop 停止出声。", false,
		func(ctx context.Context, in struct {
			ZoneID    string `json:"zone_id"`
			SpeakerID string `json:"speaker_id"`
			Mode      string `json:"mode,omitempty"`
		}) (any, error) {
			mode := in.Mode
			if mode == "" {
				mode = "continue"
			}
			if mode != "continue" && mode != "stop" {
				return nil, invalidInput("mode 只支持 continue / stop")
			}
			if err := s.deps.Players.RemoveMember(ctx, in.ZoneID, in.SpeakerID, mode); err != nil {
				return nil, err
			}
			zoneID := s.deps.Players.GroupIDOf(in.SpeakerID)
			group, err := s.deps.Players.GetGroup(zoneID)
			if err != nil {
				return nil, err
			}
			return mutation(zoneView(group), map[string]any{"removed_from": in.ZoneID, "mode": mode}), nil
		})

	registerTool(s, ScopeSpeakers, "update_zone_member", "设置区域成员的延迟补偿、音量偏移和声道模式；仅输出能力支持的设置会实际生效。", false,
		func(ctx context.Context, in struct {
			ZoneID       string  `json:"zone_id"`
			SpeakerID    string  `json:"speaker_id"`
			DelayMs      *int    `json:"delay_ms,omitempty"`
			VolumeOffset *int    `json:"volume_offset,omitempty"`
			ChannelMode  *string `json:"channel_mode,omitempty"`
		}) (any, error) {
			group, err := s.deps.Players.GetGroup(in.ZoneID)
			if err != nil {
				return nil, err
			}
			delay, offset := 0, 0
			found := false
			for _, member := range group.Members {
				if member.PlayerID == in.SpeakerID {
					delay, offset, found = member.DelayMs, member.VolumeOffset, true
					break
				}
			}
			if !found {
				return nil, invalidInput("speaker_id 不属于该区域")
			}
			if in.DelayMs != nil {
				delay = *in.DelayMs
			}
			if in.VolumeOffset != nil {
				offset = *in.VolumeOffset
			}
			modes := []string{}
			if in.ChannelMode != nil {
				modes = append(modes, *in.ChannelMode)
			}
			updated, err := s.deps.Players.UpdateMember(ctx, in.ZoneID, in.SpeakerID, delay, offset, modes...)
			if err != nil {
				return nil, err
			}
			return mutation(zoneView(updated), nil), nil
		})

	registerTool(s, ScopeSpeakers, "reorder_zone_members", "调整区域成员顺序；speaker_ids 必须是该区域全部成员的全排列，不改变主控。", false,
		func(ctx context.Context, in struct {
			ZoneID     string   `json:"zone_id"`
			SpeakerIDs []string `json:"speaker_ids"`
		}) (any, error) {
			group, err := s.deps.Players.ReorderMembers(ctx, in.ZoneID, in.SpeakerIDs)
			if err != nil {
				return nil, err
			}
			return mutation(zoneView(group), nil), nil
		})

	registerTool(s, ScopeSpeakers, "set_zone_master", "设置区域主控音箱；目标必须是当前区域成员，播放中切换不重建解码器。", false,
		func(ctx context.Context, in struct {
			ZoneID    string `json:"zone_id"`
			SpeakerID string `json:"speaker_id"`
		}) (any, error) {
			group, err := s.deps.Players.SetMaster(ctx, in.ZoneID, in.SpeakerID)
			if err != nil {
				return nil, err
			}
			return mutation(zoneView(group), nil), nil
		})

	registerTool(s, ScopeRead, "get_zone_volume", "获取区域基准音量、静音状态和每个成员叠加偏移后的实际音量。", true,
		func(_ context.Context, in struct {
			ZoneID string `json:"zone_id"`
		}) (any, error) {
			volume, err := s.deps.Players.GroupVolumeOf(in.ZoneID)
			if err != nil {
				return nil, err
			}
			return volume, nil
		})

	registerTool(s, ScopeSpeakers, "set_zone_volume", "设置区域基准音量 0-100 或静音。逐成员应用音量偏移并返回部分失败，不回滚成功成员。", false,
		func(ctx context.Context, in struct {
			ZoneID string `json:"zone_id"`
			Volume int    `json:"volume"`
			Muted  bool   `json:"muted,omitempty"`
		}) (any, error) {
			if in.Volume < 0 || in.Volume > 100 {
				return nil, invalidInput("volume 必须在 0-100 之间")
			}
			volume, err := s.deps.Players.SetGroupVolume(ctx, in.ZoneID, in.Volume, in.Muted)
			if err != nil {
				return nil, err
			}
			result := mutation(volume, map[string]any{"volume": in.Volume, "muted": in.Muted})
			for _, member := range volume.Members {
				if !member.OK {
					result.PartialFailures = append(result.PartialFailures, member.PlayerID+": "+member.Error)
				}
			}
			return result, nil
		})
}
