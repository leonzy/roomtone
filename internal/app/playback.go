package app

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"runtime"
	"strconv"
	"strings"
	"time"

	"songloft/internal/app/ws"
	"songloft/internal/handlers"
	"songloft/internal/player"
	"songloft/internal/player/discovery"
	"songloft/internal/player/pipeline"
	"songloft/internal/player/sink"
	"songloft/internal/player/sink/dlna"
	"songloft/internal/player/sink/local"
	"songloft/internal/player/sink/raop"
	"songloft/internal/services"
)

// initPlayback 初始化播放控制与音箱输出子系统。
//
// 顺序：事件总线 → WS hub → 持久化适配 → 解码/分发 → 管理器 → handler。
// 任何一步失败都只让播放功能不可用，不能让整个服务起不来
// （Bundle 模式下 iOS 明确不支持音箱推送，见 §8.3）。
func (a *App) initPlayback() {
	bus := player.NewEventBus()
	a.playbackBus = bus
	a.wsHub = ws.NewHub(bus, slog.Default())

	store := newPlaybackStore(a.db.PlaybackRepository())

	// ffmpeg 路径每次都从配置读，改了设置立即生效。
	ffmpegPath := func() string {
		var cfg struct {
			Path string `json:"path"`
		}
		if err := a.configService.GetJSON("ffmpeg_path", &cfg); err != nil || cfg.Path == "" {
			return "ffmpeg"
		}
		return cfg.Path
	}
	decoder := pipeline.NewFFmpegDecoderFactory(ffmpegPath, slog.Default())

	runners := func(timeline *player.Timeline, targets *sink.Set, startSample int64, onProgress func(int64, time.Time)) player.FrameRunner {
		return &pipeline.Distributor{
			Rate:         timeline.SampleRate(),
			FrameSamples: pipeline.DefaultFrameSamples,
			Targets:      targets,
			Timeline:     timeline,
			StartSample:  startSample,
			OnProgress:   onProgress,
			OnUnderrun: func(n int) {
				// 欠载是"解码/磁盘跟不上"，和"某一路网络慢"是两回事，
				// 指标分开记，否则排查时只能看到一堆数字一起涨。
				if a.playerManager != nil {
					a.playerManager.Metrics().AddUnderrun(n)
				}
				slog.Debug("player underrun, padding silence", "frames", n)
			},
			OnSinkDrop: func(id string, frames int) {
				if a.playerManager != nil {
					a.playerManager.Metrics().AddDroppedFrames(int64(frames))
				}
				slog.Warn("sink queue full, dropping frames", "player_id", id, "frames", frames)
			},
		}
	}

	port := 58091
	if p, err := strconv.Atoi(a.config.Port); err == nil {
		port = p
	}
	a.lanURL = services.NewLANURLResolver(port)

	var output handlers.PlayerOutputSetting
	if err := a.configService.GetJSON("player_output", &output); err != nil || output.MaxGroups == 0 {
		output.MaxGroups = player.DefaultMaxGroups
		output.PartyEnabled = true
		output.PartyDriftToleranceMs = 1500
		output.DefaultLatencyMs = 2000
	}
	budget := player.NewBudget(output.MaxGroups)

	resolver := songTrackResolver{get: func(ctx context.Context, id int64) (string, string, string, float64, error) {
		song, err := a.songService.GetByID(ctx, id)
		if err != nil {
			return "", "", "", 0, err
		}
		return song.Title, song.FilePath, song.URL, song.Duration, nil
	}}

	latency := time.Duration(output.DefaultLatencyMs) * time.Millisecond
	a.playerManager = player.NewManager(player.ManagerDeps{
		Store:                 store,
		Bus:                   bus,
		Budget:                budget,
		Resolver:              resolver,
		Decoder:               decoder,
		Runners:               runners,
		Builder:               &playerSinkBuilder{hub: a.wsHub, lan: a.lanURL},
		Logger:                slog.Default(),
		Latency:               latency,
		PartyEnabled:          output.PartyEnabled,
		PartyDriftToleranceMs: output.PartyDriftToleranceMs,
	})

	// 恢复上次的设备 / 组 / 队列。失败不阻塞启动，只是没有断点续播。
	if err := a.playerManager.Restore(context.Background()); err != nil {
		slog.Warn("恢复播放状态失败", "err", err)
	}

	a.playbackHandler = handlers.NewPlaybackHandler(a.playerManager, a.wsHub)
	// Bundle / 移动端限制（X-04）：iOS 上 Go 后端跑在 app 进程内，推流目标
	// 会跟着 App 一起被系统挂起，所以默认只留 local sink；桌面 Bundle 由
	// 用户显式打开 network_sinks_enabled。拒绝时把原因原样返回，前端照抄提示。
	a.playbackHandler.SetNetworkSinkPolicy(func() (bool, string) {
		if runtime.GOOS == "ios" {
			return false, "iOS 内嵌模式下不支持网络音箱（AirPlay / DLNA），请用本机播放或在服务器上部署"
		}
		cfg := handlers.PlayerOutputSetting{NetworkSinksEnabled: true}
		if err := a.configService.GetJSON("player_output", &cfg); err != nil {
			return true, ""
		}
		if !cfg.NetworkSinksEnabled {
			return false, "已在设置中关闭网络音箱（/settings/player-output 的 network_sinks_enabled）"
		}
		return true, ""
	})

	// 播放器出口白名单：每次校验现读配置，改完不用重启（与代理白名单各自独立）。
	a.playbackHandler.SetOutputAllowlist(func() []*net.IPNet {
		var cfg struct {
			Entries []string `json:"entries"`
		}
		if err := a.configService.GetJSON("player_output_allowlist", &cfg); err != nil {
			return nil
		}
		nets, err := services.ParsePlayerOutputAllowlist(cfg.Entries)
		if err != nil {
			return nil
		}
		return nets
	})
	a.wsHub.OnMessage(a.handlePlayerMessage)

	// 设备发现：mDNS 找 AirPlay 1 音箱，SSDP 找 DLNA renderer。
	// 扫到的设备直接登记成 player（单机也建只含自己的组），前端从
	// GET /players 就能看到，不必手填 IP。
	disc := handlers.PlayerDiscoverySetting{Enabled: true, Protocols: []string{"raop", "dlna"}, IntervalSeconds: 30}
	if err := a.configService.GetJSON("player_discovery", &disc); err != nil {
		slog.Debug("读取设备发现配置失败，使用默认值", "err", err)
	}
	a.playerDiscovery = discovery.New(discovery.Options{
		Registry: a.playerManager,
		Scanners: []discovery.Scanner{
			&discovery.RAOPScanner{Timeout: 2 * time.Second},
			&discovery.DLNAScanner{Timeout: 3 * time.Second},
		},
		Logger:    slog.Default(),
		Enabled:   disc.Enabled,
		Protocols: disc.Protocols,
		Interval:  time.Duration(disc.IntervalSeconds) * time.Second,
	})
	a.playerDiscovery.Start()
	a.playbackHandler.SetDiscovery(a.playerDiscovery)
	a.playbackHandler.SetAddressPolicy(func() bool {
		var out handlers.PlayerOutputSetting
		if err := a.configService.GetJSON("player_output", &out); err != nil {
			return true
		}
		return out.AllowPrivateNetwork
	})
}

// applyPlayerDiscoveryConfig 把 /settings/player-discovery 的最新值推给发现器。
//
// 两个入口都调它：业务端点 PUT 后立即触发，admin 直改 /configs 走
// configHandler 的 onConfigChanged。副作用必须两边一致（AGENTS.md 铁律）。
func (a *App) applyPlayerDiscoveryConfig() {
	if a.playerDiscovery == nil {
		return
	}
	cfg := handlers.PlayerDiscoverySetting{Enabled: true, Protocols: []string{"raop", "dlna"}, IntervalSeconds: 30}
	if err := a.configService.GetJSON("player_discovery", &cfg); err != nil {
		slog.Debug("读取设备发现配置失败，使用默认值", "err", err)
	}
	a.playerDiscovery.ApplyConfig(cfg.Enabled, cfg.Protocols, time.Duration(cfg.IntervalSeconds)*time.Second)
}

// playerSinkBuilder 按设备类型造 sink。
type playerSinkBuilder struct {
	hub *ws.Hub
	lan *services.LANURLResolver
}

// Build 实现 player.SinkBuilder。
//
// 每次起播都会走一遍：RAOP 走 RTSP 握手，DLNA 先取 desc.xml 再决定
// 用哪套服务端点。网络类失败一律包成 ErrPlayerOffline，交给上层
// 标离线并重试，而不是让整个播放请求 500。
func (b *playerSinkBuilder) Build(ctx context.Context, p *player.Player) (sink.Sink, error) {
	switch p.Kind {
	case player.KindLocal:
		sender, ok := b.hub.Sender(p.ID)
		if !ok {
			return nil, fmt.Errorf("%w: 客户端未连接", player.ErrPlayerOffline)
		}
		return local.New(p.ID, localController{c: sender.(*ws.Client)}, p.Caps), nil

	case player.KindRAOP:
		if p.Address == "" {
			return nil, fmt.Errorf("%w: 音箱地址为空", player.ErrPlayerOffline)
		}
		rate, ch := outputFormat(p.Caps)
		return raop.New(p.ID, raop.Options{
			Address:     p.Address,
			DeviceID:    p.Caps.DeviceID,
			PublicKey:   p.Caps.PublicKey,
			Encrypted:   p.Caps.PublicKey != "",
			SampleRate:  rate,
			Channels:    ch,
			DialTimeout: 5 * time.Second,
		}), nil

	case player.KindDLNA:
		if p.Address == "" {
			return nil, fmt.Errorf("%w: 设备地址为空", player.ErrPlayerOffline)
		}
		cl := dlna.NewClient()
		fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		dev, err := dlna.Fetch(fetchCtx, cl, p.Address)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("%w: %v", player.ErrPlayerOffline, err)
		}
		return dlna.New(p.ID, dev, cl, dlna.Options{CallbackBase: b.callbackBase(ctx, p.Address)}), nil
	}
	return nil, fmt.Errorf("%w: 暂不支持 %s 设备", player.ErrUnsupported, p.Kind)
}

// callbackBase GENA 事件回调的基地址；拿不到就返回空，sink 会退回轮询。
func (b *playerSinkBuilder) callbackBase(ctx context.Context, target string) string {
	if b.lan == nil {
		return ""
	}
	u, err := b.lan.Resolve(ctx, target, "/", "")
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(u, "/")
}

// outputFormat 从设备能力里取协商格式，缺省 44.1k/2ch。
func outputFormat(c player.Caps) (rate, channels int) {
	rate, channels = c.MaxSampleRate, c.Channels
	if rate <= 0 {
		rate = 44100
	}
	if channels <= 0 {
		channels = 2
	}
	return rate, channels
}

// localController 把 ws.Client 适配成 local.Controller。
//
// 单独一层是为了让 local 包不依赖 gorilla/websocket：sink 只需要
// "把一条指令发出去"这个能力，具体走 WS 还是别的通道是宿主的事。
type localController struct {
	c *ws.Client
}

func (l localController) SendCommand(cmd local.Command) error {
	return l.c.SendCommand(cmd)
}

// handlePlayerMessage 处理客户端 WS 上行消息。
//
// 目前只有两种：register_player（绑定连接与设备）与 report（500ms 状态上报）。
// 消息格式刻意保持极简，客户端 SDK 直接拼 JSON 即可。
func (a *App) handlePlayerMessage(c *ws.Client, payload []byte) {
	var msg struct {
		Type string `json:"type"`
		// register_player
		ID      string   `json:"id"`
		Name    string   `json:"name"`
		Kind    string   `json:"kind"`
		Codecs  []string `json:"codecs"`
		MaxRate int      `json:"max_sample_rate"`
		// report
		PositionMs int64  `json:"position_ms"`
		Buffering  bool   `json:"buffering"`
		Volume     int    `json:"volume"`
		State      string `json:"state"`
		RTTMs      int64  `json:"rtt_ms"`
	}
	if err := jsonUnmarshal(payload, &msg); err != nil {
		slog.Debug("player ws message decode failed", "err", err)
		return
	}
	switch msg.Type {
	case "register_player":
		caps := player.Caps{
			MaxSampleRate:  msg.MaxRate,
			Channels:       2,
			BitDepth:       16,
			SupportsVolume: true,
			SupportsSeek:   true,
			LatencyMs:      300,
			SyncTier:       player.SyncExact,
		}
		for _, c := range msg.Codecs {
			caps.Codecs = append(caps.Codecs, player.Codec(c))
		}
		kind := player.Kind(msg.Kind)
		if kind == "" {
			kind = player.KindLocal
		}
		p := player.Player{ID: msg.ID, Kind: kind, Name: msg.Name, Caps: caps}
		if p.Name == "" {
			p.Name = "本机播放器"
		}
		if err := a.playerManager.RegisterPlayer(context.Background(), p); err != nil {
			slog.Warn("注册播放设备失败", "id", msg.ID, "err", err)
			return
		}
		a.wsHub.RegisterDevice(c, msg.ID)
	case "report":
		if sender, ok := a.wsHub.Sender(c.PlayerID()); ok {
			_ = sender
		}
		if p := c.PlayerID(); p != "" {
			if s, ok := a.playerManager.SinkOf(p); ok {
				if ls, ok := s.(*local.Sink); ok {
					ls.HandleReport(local.Report{
						PositionMs: msg.PositionMs,
						Buffering:  msg.Buffering,
						Volume:     msg.Volume,
						State:      msg.State,
						RTTMs:      msg.RTTMs,
					})
				}
			}
		}
	}
}
