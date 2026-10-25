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
	"songloft/internal/player/sink/raop"
)

// RAOPScanner 通过 mDNS 发现 AirPlay 1（RAOP）音箱。
type RAOPScanner struct {
	// Timeout 一轮浏览的等待时长，默认 2s。
	Timeout time.Duration
	// Query 注入用；nil 走真实 mDNS。
	Query raop.Query
}

// Kind 实现 Scanner。
func (s *RAOPScanner) Kind() player.Kind { return player.KindRAOP }

// Scan 实现 Scanner。
func (s *RAOPScanner) Scan(ctx context.Context) ([]Speaker, error) {
	found, err := raop.Discover(ctx, s.Query, s.Timeout)
	if err != nil {
		return nil, err
	}
	out := make([]Speaker, 0, len(found))
	for _, f := range found {
		out = append(out, Speaker{
			// id 前缀与 players.id 的约定一致：raop:AA:BB:...
			ID:      "raop:" + colonMAC(f.DeviceID),
			Kind:    player.KindRAOP,
			Name:    f.Name,
			Model:   f.Model,
			Address: f.Address,
			Caps:    f.Caps,
		})
	}
	return out, nil
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
