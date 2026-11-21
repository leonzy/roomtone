package mcpserver

import (
	"context"
	"time"

	"songloft/internal/handlers"
)

func (s *Server) playbackSettings() map[string]any {
	discovery := handlers.PlayerDiscoverySetting{Enabled: true, Protocols: []string{"raop", "dlna"}, IntervalSeconds: 30}
	if err := s.deps.Configs.GetJSON("player_discovery", &discovery); err != nil {
		discovery = handlers.PlayerDiscoverySetting{Enabled: true, Protocols: []string{"raop", "dlna"}, IntervalSeconds: 30}
	}
	output := defaultOutputSetting()
	if err := s.deps.Configs.GetJSON("player_output", &output); err != nil {
		output = defaultOutputSetting()
	}
	replayGain := handlers.ReplayGainSetting{Mode: "off"}
	_ = s.deps.Configs.GetJSON(handlers.ReplayGainKey, &replayGain)
	crossfade := handlers.CrossfadeSetting{Seconds: 3}
	_ = s.deps.Configs.GetJSON("crossfade", &crossfade)
	return map[string]any{"discovery": discovery, "output": output, "replaygain": replayGain, "crossfade": crossfade}
}

func defaultOutputSetting() handlers.PlayerOutputSetting {
	return handlers.PlayerOutputSetting{MaxGroups: 4, DefaultLatencyMs: 2000, AllowPrivateNetwork: true,
		NetworkSinksEnabled: true, PartyEnabled: true, PartyDriftToleranceMs: 1500}
}

func (s *Server) registerSettingsTools() {
	registerTool(s, ScopeRead, "get_playback_settings", "读取设备发现、播放输出、ReplayGain 和交叉淡化配置；不读取或修改出口网络安全白名单。", true,
		func(_ context.Context, _ struct{}) (any, error) {
			return s.playbackSettings(), nil
		})

	registerTool(s, ScopeSpeakers, "update_playback_settings", "更新设备发现、输出、ReplayGain 或交叉淡化配置。未提供的配置组保持不变；不允许通过 MCP 修改私网访问安全策略。", false,
		func(_ context.Context, in struct {
			Discovery  *handlers.PlayerDiscoverySetting `json:"discovery,omitempty"`
			Output     *handlers.PlayerOutputSetting    `json:"output,omitempty"`
			ReplayGain *handlers.ReplayGainSetting      `json:"replaygain,omitempty"`
			Crossfade  *handlers.CrossfadeSetting       `json:"crossfade,omitempty"`
		}) (any, error) {
			if in.Discovery == nil && in.Output == nil && in.ReplayGain == nil && in.Crossfade == nil {
				return nil, invalidInput("至少提供一个配置组")
			}
			if in.Discovery != nil {
				cfg := *in.Discovery
				if cfg.IntervalSeconds == 0 {
					cfg.IntervalSeconds = 30
				}
				if cfg.IntervalSeconds < 10 || cfg.IntervalSeconds > 600 {
					return nil, invalidInput("discovery.interval_seconds 必须在 10-600 之间")
				}
				if len(cfg.Protocols) == 0 {
					cfg.Protocols = []string{"raop", "dlna"}
				}
				for _, protocol := range cfg.Protocols {
					if protocol != "raop" && protocol != "raat" && protocol != "dlna" {
						return nil, invalidInput("发现协议只支持 raop / raat / dlna")
					}
				}
				if err := s.deps.Configs.SetJSON("player_discovery", cfg); err != nil {
					return nil, err
				}
				if runner, ok := s.deps.Discovery.(interface {
					ApplyConfig(bool, []string, time.Duration)
				}); ok {
					runner.ApplyConfig(cfg.Enabled, cfg.Protocols, time.Duration(cfg.IntervalSeconds)*time.Second)
				}
			}
			if in.Output != nil {
				current := defaultOutputSetting()
				_ = s.deps.Configs.GetJSON("player_output", &current)
				cfg := *in.Output
				cfg.AllowPrivateNetwork = current.AllowPrivateNetwork
				if cfg.MaxGroups == 0 {
					cfg.MaxGroups = 4
				}
				if cfg.MaxGroups < 1 || cfg.MaxGroups > 16 {
					return nil, invalidInput("output.max_groups 必须在 1-16 之间")
				}
				if cfg.DefaultLatencyMs <= 0 {
					cfg.DefaultLatencyMs = 2000
				}
				if cfg.PartyDriftToleranceMs == 0 {
					cfg.PartyDriftToleranceMs = 1500
				}
				if cfg.PartyDriftToleranceMs < 500 || cfg.PartyDriftToleranceMs > 5000 || cfg.DLNARealignIntervalMinutes < 0 {
					return nil, invalidInput("output 的派对容忍或 DLNA 重对齐参数不合法")
				}
				if err := s.deps.Configs.SetJSON("player_output", cfg); err != nil {
					return nil, err
				}
			}
			if in.ReplayGain != nil {
				switch in.ReplayGain.Mode {
				case "off", "track", "album":
				default:
					return nil, invalidInput("replaygain.mode 只支持 off / track / album")
				}
				if err := s.deps.Configs.SetJSON(handlers.ReplayGainKey, in.ReplayGain); err != nil {
					return nil, err
				}
			}
			if in.Crossfade != nil {
				if in.Crossfade.Enabled && (in.Crossfade.Seconds < 0.5 || in.Crossfade.Seconds > 12) {
					return nil, invalidInput("crossfade.seconds 必须在 0.5-12 之间")
				}
				if err := s.deps.Configs.SetJSON("crossfade", in.Crossfade); err != nil {
					return nil, err
				}
			}
			return mutation(s.playbackSettings(), nil), nil
		})
}
