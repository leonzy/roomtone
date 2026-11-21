package mcpserver

import (
	"context"

	"songloft/internal/player"
	"songloft/internal/player/discovery"
)

type SpeakerAdmin interface {
	AddSpeaker(ctx context.Context, kind, name, address, deviceID, publicKey string) (player.Player, error)
	ReconnectSpeaker(ctx context.Context, id string) (player.Player, error)
}

func (s *Server) speaker(id string) (player.Player, error) {
	p, err := s.deps.Players.GetPlayer(id)
	return p, err
}

func (s *Server) registerSpeakerTools() {
	registerTool(s, ScopeRead, "list_speakers", "列出全部播放音箱及其在线状态、所属区域、音量、音量上限、EQ 和安全的能力摘要；不返回密码或配对密钥。", true,
		func(_ context.Context, _ struct{}) (any, error) {
			players := s.deps.Players.ListPlayers()
			out := make([]SpeakerView, 0, len(players))
			for _, p := range players {
				out = append(out, speakerView(p, s.deps.Players.GroupIDOf(p.ID)))
			}
			return map[string]any{"speakers": out, "total": len(out)}, nil
		})

	registerTool(s, ScopeSpeakers, "discover_speakers", "立即执行一轮 AirPlay/RAOP、RAAT 或 DLNA 设备扫描并登记设备；实际协议由现有发现配置决定。", false,
		func(ctx context.Context, _ struct{}) (any, error) {
			if s.deps.Discovery == nil {
				return nil, &ToolError{Code: "DISCOVERY_UNAVAILABLE", Message: "当前运行模式不支持设备发现"}
			}
			result, err := s.deps.Discovery.Scan(ctx)
			if err != nil {
				return nil, err
			}
			return discoveryResultView(result), nil
		})

	registerTool(s, ScopeSpeakers, "add_speaker", "按地址手动添加 RAOP 或 DLNA 音箱。RAOP 需要 device_id；DLNA 会读取设备描述验证能力。", false,
		func(ctx context.Context, in struct {
			Kind      string `json:"kind"`
			Name      string `json:"name,omitempty"`
			Address   string `json:"address"`
			DeviceID  string `json:"device_id,omitempty"`
			PublicKey string `json:"public_key,omitempty"`
		}) (any, error) {
			if s.deps.SpeakerAdmin == nil {
				return nil, &ToolError{Code: "UNSUPPORTED_OPERATION", Message: "手动添加音箱不可用"}
			}
			p, err := s.deps.SpeakerAdmin.AddSpeaker(ctx, in.Kind, in.Name, in.Address, in.DeviceID, in.PublicKey)
			if err != nil {
				return nil, err
			}
			return mutation(speakerView(p, s.deps.Players.GroupIDOf(p.ID)), nil), nil
		})

	registerTool(s, ScopeSpeakers, "update_speaker", "更新音箱名称、接管策略、音量模式、音量上限、DLNA 延迟或设备密码；未提供字段保持不变。", false,
		func(ctx context.Context, in struct {
			SpeakerID        string  `json:"speaker_id"`
			Name             *string `json:"name,omitempty"`
			TakeoverBehavior *string `json:"takeover_behavior,omitempty"`
			VolumeMode       *string `json:"volume_mode,omitempty"`
			MaxVolume        *int    `json:"max_volume,omitempty"`
			DLNASyncDelayMs  *int    `json:"dlna_sync_delay_ms,omitempty"`
			Password         *string `json:"password,omitempty"`
		}) (any, error) {
			if in.SpeakerID == "" {
				return nil, invalidInput("speaker_id 不能为空")
			}
			upd := player.PlayerUpdate{Name: in.Name, VolumeMode: in.VolumeMode, MaxVolume: in.MaxVolume, DLNASyncDelayMs: in.DLNASyncDelayMs, Password: in.Password}
			if in.TakeoverBehavior != nil {
				value := player.TakeoverBehavior(*in.TakeoverBehavior)
				upd.Takeover = &value
			}
			p, err := s.deps.Players.UpdatePlayer(ctx, in.SpeakerID, upd)
			if err != nil {
				return nil, err
			}
			return mutation(speakerView(p, s.deps.Players.GroupIDOf(p.ID)), nil), nil
		})

	registerTool(s, ScopeSpeakers, "remove_speaker", "删除明确 speaker_id 的设备登记，并将其移出分组；不会删除任何歌曲或宿主机文件。", false,
		func(ctx context.Context, in struct {
			SpeakerID string `json:"speaker_id"`
		}) (any, error) {
			if in.SpeakerID == "" {
				return nil, invalidInput("speaker_id 不能为空")
			}
			p, err := s.speaker(in.SpeakerID)
			if err != nil {
				return nil, err
			}
			if err := s.deps.Players.DeletePlayer(ctx, in.SpeakerID); err != nil {
				return nil, err
			}
			return mutation(map[string]any{"deleted": in.SpeakerID}, map[string]any{"name": p.Name}), nil
		})

	registerTool(s, ScopeSpeakers, "reconnect_speaker", "丢弃音箱缓存连接并重新握手；失败时保持离线状态。", false,
		func(ctx context.Context, in struct {
			SpeakerID string `json:"speaker_id"`
		}) (any, error) {
			if s.deps.SpeakerAdmin == nil {
				return nil, &ToolError{Code: "UNSUPPORTED_OPERATION", Message: "音箱重连不可用"}
			}
			p, err := s.deps.SpeakerAdmin.ReconnectSpeaker(ctx, in.SpeakerID)
			if err != nil {
				return nil, err
			}
			return mutation(speakerView(p, s.deps.Players.GroupIDOf(p.ID)), nil), nil
		})

	registerTool(s, ScopeSpeakers, "set_speaker_volume", "设置单台音箱音量 0-100 和静音状态；遵守 fixed 音量模式与 max_volume 上限，返回实际值。", false,
		func(ctx context.Context, in struct {
			SpeakerID string `json:"speaker_id"`
			Volume    int    `json:"volume"`
			Muted     bool   `json:"muted,omitempty"`
		}) (any, error) {
			if in.Volume < 0 || in.Volume > 100 {
				return nil, invalidInput("volume 必须在 0-100 之间")
			}
			p, err := s.deps.Players.SetVolume(ctx, in.SpeakerID, in.Volume, in.Muted)
			if err != nil {
				return nil, err
			}
			result := mutation(speakerView(p, s.deps.Players.GroupIDOf(p.ID)), map[string]any{"requested_volume": in.Volume, "volume": p.Volume, "muted": p.Muted})
			if p.VolumeMode == player.VolumeModeFixed {
				result.Warnings = append(result.Warnings, "设备使用固定音量模式，请求未改变音量")
			} else if p.Volume != in.Volume {
				result.Warnings = append(result.Warnings, "音量已被设备上限钳位")
			}
			return result, nil
		})

	registerTool(s, ScopeSpeakers, "set_speaker_power", "设置音箱播放待机或唤醒。on=false 停止向设备输出；不宣称执行了物理关机。", false,
		func(ctx context.Context, in struct {
			SpeakerID string `json:"speaker_id"`
			On        bool   `json:"on"`
		}) (any, error) {
			p, err := s.deps.Players.SetPower(ctx, in.SpeakerID, in.On)
			if err != nil {
				return nil, err
			}
			return mutation(speakerView(p, s.deps.Players.GroupIDOf(p.ID)), map[string]any{"on": in.On}), nil
		})

	registerTool(s, ScopeRead, "get_speaker_eq", "获取单台音箱的 10 段 EQ、预设、自动前级增益和手动衰减。", true,
		func(_ context.Context, in struct {
			SpeakerID string `json:"speaker_id"`
		}) (any, error) {
			p, err := s.speaker(in.SpeakerID)
			if err != nil {
				return nil, err
			}
			return p.EQ, nil
		})

	registerTool(s, ScopeRead, "list_eq_presets", "列出服务端内置 10 段 EQ 预设及自动防削波前级增益。", true,
		func(_ context.Context, _ struct{}) (any, error) {
			presets := player.EQPresets()
			return map[string]any{"presets": presets, "total": len(presets)}, nil
		})

	registerTool(s, ScopeSpeakers, "set_speaker_eq", "按预设 ID 或自定义 10 段 bands 设置音箱 EQ；preset 与 bands 互斥，attenuation_db 可选 0-24dB。", false,
		func(ctx context.Context, in struct {
			SpeakerID     string    `json:"speaker_id"`
			Preset        string    `json:"preset,omitempty"`
			Enabled       *bool     `json:"enabled,omitempty"`
			Bands         []float64 `json:"bands,omitempty"`
			AttenuationDB *float64  `json:"attenuation_db,omitempty"`
		}) (any, error) {
			if in.Preset != "" && len(in.Bands) > 0 {
				return nil, invalidInput("preset 与 bands 不能同时设置")
			}
			p, err := s.speaker(in.SpeakerID)
			if err != nil {
				return nil, err
			}
			eq := p.EQ
			if in.Preset != "" {
				preset, ok := player.EQPresetByID(in.Preset)
				if !ok {
					return nil, invalidInput("未知 EQ 预设 %q", in.Preset)
				}
				eq.Enabled, eq.Preset, eq.Bands = true, preset.ID, preset.Bands
			}
			if len(in.Bands) > 0 {
				if len(in.Bands) != player.EQBandCount {
					return nil, invalidInput("bands 必须包含 10 段")
				}
				eq.Bands, eq.Preset = in.Bands, "custom"
			}
			if in.Enabled != nil {
				eq.Enabled = *in.Enabled
			}
			if in.AttenuationDB != nil {
				if *in.AttenuationDB < 0 || *in.AttenuationDB > 24 {
					return nil, invalidInput("attenuation_db 必须在 0-24 之间")
				}
				eq.AttenuationDB = *in.AttenuationDB
			}
			out, err := s.deps.Players.SetEQ(ctx, in.SpeakerID, eq)
			if err != nil {
				return nil, err
			}
			return mutation(out, nil), nil
		})
}

func discoveryResultView(result discovery.Result) map[string]any {
	speakers := make([]map[string]any, 0, len(result.Speakers))
	for _, speaker := range result.Speakers {
		speakers = append(speakers, map[string]any{
			"id": speaker.ID, "kind": speaker.Kind, "name": speaker.Name,
			"model": speaker.Model, "sync_tier": speaker.Caps.SyncTier.String(),
			"airplay2": speaker.Caps.AirPlay2,
		})
	}
	return map[string]any{
		"speakers": speakers, "added": result.Added, "updated": result.Updated,
		"skipped": result.Skipped, "errors": result.Errors, "scanned_at": result.ScannedAt,
		"total": len(speakers),
	}
}
