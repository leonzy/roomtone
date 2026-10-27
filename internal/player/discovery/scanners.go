package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"songloft/internal/player"
	"songloft/internal/player/sink/dlna"
	"songloft/internal/player/sink/raat"
	"songloft/internal/player/sink/raop"
	"songloft/internal/player/sink/raop2"
)

// RAOPScanner 通过 mDNS 发现 AirPlay 音箱：`_raop._tcp`（V1）+
// `_airplay._tcp`（V2，含设备侧编组），同一物理设备的两个服务按
// DeviceID 关联合并。
type RAOPScanner struct {
	// Timeout 一轮浏览的等待时长，默认 2s。
	Timeout time.Duration
	// Query 注入用；nil 走真实 mDNS。
	Query raop.Query
	// QueryAirPlay `_airplay._tcp` 查询（V2 发现）；nil 走真实 mDNS。
	QueryAirPlay raop2.Query
}

// Kind 实现 Scanner。
func (s *RAOPScanner) Kind() player.Kind { return player.KindRAOP }

// Scan 实现 Scanner。
func (s *RAOPScanner) Scan(ctx context.Context) ([]Speaker, error) {
	found, err := raop.Discover(ctx, s.Query, s.Timeout)
	if err != nil {
		return nil, err
	}
	// V2 发现（`_airplay._tcp`）：失败只降级记日志（V1 设备照常可用）。
	v2Devs, err := raop2.Discover(ctx, s.QueryAirPlay, s.Timeout)
	if err != nil {
		slog.Warn("_airplay._tcp 发现失败（仅 V1 可用）", "err", err)
		v2Devs = nil
	}
	v2ByDevice := make(map[string]raop2.Device, len(v2Devs))
	for _, d := range v2Devs {
		v2ByDevice[normalizeMAC(d.DeviceID)] = d
	}

	out := make([]Speaker, 0, len(found)+len(v2Devs))
	seenV2 := map[string]bool{}
	for _, f := range found {
		sp := Speaker{
			// id 前缀与 players.id 的约定一致：raop:AA:BB:...
			ID:      "raop:" + colonMAC(f.DeviceID),
			Kind:    player.KindRAOP,
			Name:    f.Name,
			Model:   f.Model,
			Address: f.Address,
			Caps:    f.Caps,
		}
		// 同一物理设备的 _airplay._tcp 服务：合并 V2 能力（A2P-6）
		if d, ok := v2ByDevice[normalizeMAC(f.DeviceID)]; ok {
			seenV2[normalizeMAC(f.DeviceID)] = true
			mergeAirPlay2(&sp, d)
		}
		out = append(out, sp)
	}
	// 只有 _airplay._tcp（无 _raop._tcp）的设备：V2-only（如 HomePod 立体声组长）
	for _, d := range v2Devs {
		key := normalizeMAC(d.DeviceID)
		if seenV2[key] {
			continue
		}
		sp := Speaker{
			ID:      "raop:" + colonMAC(d.DeviceID),
			Kind:    player.KindRAOP,
			Name:    d.Name,
			Model:   d.Model,
			Address: d.Address,
		}
		mergeAirPlay2(&sp, d)
		out = append(out, sp)
	}
	return out, nil
}

// mergeAirPlay2 把 V2 发现的能力合并进 Speaker（Caps.AirPlay2、密码、
// features 位 → SupportsArtwork/BPlistNowPlaying 等、编组信息进 Model）。
func mergeAirPlay2(sp *Speaker, d raop2.Device) {
	sp.Caps.AirPlay2 = true
	sp.Caps.NeedsPassword = sp.Caps.NeedsPassword || d.RequiresPassword
	if d.PublicKey != "" {
		sp.Caps.PublicKey = d.PublicKey
	}
	if d.RequiresOneTimePairing || d.HasFeature(raop2.FeatureSupportsTransientPairing) {
		sp.Caps.RequiresPairing = true
	}
	if d.HasFeature(raop2.FeatureSupportsArtwork) {
		sp.Caps.SupportsArtwork = true
	}
	if d.HasFeature(raop2.FeatureSupportsBPlistNowPlaying) {
		sp.Caps.SupportsBPlistNowPlaying = true
	}
	if d.Model != "" {
		sp.Model = d.Model
	}
}

// normalizeMAC 去冒号小写（DeviceID 归一化比对）。
func normalizeMAC(s string) string {
	return strings.ToLower(strings.ReplaceAll(s, ":", ""))
}

// DLNAScanner 通过 SSDP 发现 DLNA renderer，并取 desc.xml 补名称与服务表。
//
// 只发 M-SEARCH 拿不到 friendlyName（很多设备的 SERVER 头只有型号），
// 也没法确认设备是否真有 AVTransport，所以每条响应都要再取一次描述。
type DLNAScanner struct {
	// Timeout SSDP 收包时长，默认 3s。
	Timeout time.Duration
	// Client SOAP/描述用的裸客户端。
	Client *dlna.Client
	// SSDP / Fetch 注入用；nil 走真实实现。
	SSDP  func(ctx context.Context, opts dlna.SSDPOptions) ([]dlna.SSDPDevice, error)
	Fetch func(ctx context.Context, cl *dlna.Client, address string) (*dlna.Device, error)
	// Concurrency 同时取描述的上限，默认 4（局域网设备对并发很敏感）。
	Concurrency int
}

// Kind 实现 Scanner。
func (s *DLNAScanner) Kind() player.Kind { return player.KindDLNA }

// Scan 实现 Scanner。
func (s *DLNAScanner) Scan(ctx context.Context) ([]Speaker, error) {
	ssdp := s.SSDP
	if ssdp == nil {
		ssdp = dlna.DiscoverSSDP
	}
	fetch := s.Fetch
	if fetch == nil {
		fetch = dlna.Fetch
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	cl := s.Client
	if cl == nil {
		cl = dlna.NewClient()
	}

	devices, err := ssdp(ctx, dlna.SSDPOptions{Timeout: timeout})
	if err != nil && len(devices) == 0 {
		return nil, err
	}

	// 同一台设备可能既回 MediaRenderer 又回 ssdp:all，按 UUID 去重。
	candidates := make([]dlna.SSDPDevice, 0, len(devices))
	seen := make(map[string]bool, len(devices))
	for _, d := range devices {
		if d.Location == nil || !dlna.IsMediaRenderer(d.ST, d.USN) {
			continue
		}
		key := d.UUID
		if key == "" {
			key = d.Location.String()
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		candidates = append(candidates, d)
	}

	limit := s.Concurrency
	if limit <= 0 {
		limit = 4
	}
	sem := make(chan struct{}, limit)
	var (
		mu    sync.Mutex
		out   []Speaker
		fails []string
		wg    sync.WaitGroup
	)
	for _, cand := range candidates {
		wg.Add(1)
		go func(cand dlna.SSDPDevice) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			dev, err := fetch(ctx, cl, cand.Location.String())
			if err != nil {
				mu.Lock()
				fails = append(fails, fmt.Sprintf("%s: %v", cand.Location.Host, err))
				mu.Unlock()
				return
			}
			uuid := dev.UUID()
			if uuid == "" {
				uuid = cand.UUID
			}
			if uuid == "" {
				uuid = cand.Location.Host
			}
			name := dev.FriendlyName
			if name == "" {
				name = cand.Location.Host
			}
			sp := Speaker{
				ID:      "dlna:" + uuid,
				Kind:    player.KindDLNA,
				Name:    name,
				Model:   dev.ModelName,
				Address: dev.Location.String(),
				Caps:    dlna.CapsFor(dev),
			}
			mu.Lock()
			out = append(out, sp)
			mu.Unlock()
		}(cand)
	}
	wg.Wait()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].ID < out[j].ID
		}
		return out[i].Name < out[j].Name
	})
	if len(out) == 0 && len(fails) > 0 {
		return nil, fmt.Errorf("dlna: 全部设备描述获取失败（%s）", strings.Join(fails, "; "))
	}
	for _, f := range fails {
		slog.Debug("dlna 设备描述获取失败", "detail", f)
	}
	return out, nil
}

// colonMAC 把 12 位十六进制 MAC 写成 aa:bb:cc:dd:ee:ff，便于 UI 展示与排障。
func colonMAC(id string) string {
	if len(id) != 12 {
		return id
	}
	parts := make([]string, 0, 6)
	for i := 0; i < 12; i += 2 {
		parts = append(parts, id[i:i+2])
	}
	return strings.Join(parts, ":")
}

// RAATScanner 通过 SOOD 发现 RAAT 设备，并做一次 TCP info 探测取精确能力
// （对齐 Roon 常连接取能力的思路，避免"发现期只有保守 44.1k 假能力"）。
type RAATScanner struct {
	// Timeout SOOD 发现与 info 探测的超时。
	Timeout time.Duration
	// Discover 注入用；nil 走真实 SOOD。
	Discover func(ctx context.Context, timeout time.Duration) ([]raat.Device, error)
	// ProbeInfo 注入用；nil 走真实 TCP info 探测。
	ProbeInfo func(ctx context.Context, addr string, timeout time.Duration) (raat.Info, error)
}

// Kind 实现 Scanner。
func (s *RAATScanner) Kind() player.Kind { return player.KindRAAT }

// Scan 实现 Scanner。info 探测失败（超时）的设备跳过，不整体报错。
func (s *RAATScanner) Scan(ctx context.Context) ([]Speaker, error) {
	discover := s.Discover
	if discover == nil {
		discover = func(ctx context.Context, timeout time.Duration) ([]raat.Device, error) {
			return raat.Discover(ctx, timeout, nil)
		}
	}
	probe := s.ProbeInfo
	if probe == nil {
		probe = raat.ProbeInfo
	}
	devs, err := discover(ctx, s.Timeout)
	if err != nil {
		return nil, err
	}
	out := make([]Speaker, 0, len(devs))
	for _, d := range devs {
		info, err := probe(ctx, d.Addr(), s.Timeout)
		if err != nil {
			// 探测失败：跳过该设备（不整体报错）
			slog.Debug("raat: info 探测失败，跳过设备", "device", d.UniqueID, "err", err)
			continue
		}
		rate, bits, channels := raat.ParseSupportedFormats(info.SupportedFormats)
		if rate == 0 {
			rate, bits, channels = 44100, 16, 2 // 保守缺省
		}
		sp := Speaker{
			// id 前缀与 players.id 的约定一致：raat:<unique_id>
			ID:      "raat:" + d.UniqueID,
			Kind:    player.KindRAAT,
			Name:    d.Name,
			Model:   d.Model,
			Address: d.Addr(),
			Caps: player.Caps{
				Codecs:         []player.Codec{player.CodecL16},
				MaxSampleRate:  rate,
				Channels:       channels,
				BitDepth:       bits,
				SupportsVolume: info.VolumeSupported,
				SupportsSeek:   true,
				LatencyMs:      2000,
				SyncTier:       player.SyncExact,
			},
		}
		out = append(out, sp)
	}
	return out, nil
}
