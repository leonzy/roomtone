// Package discovery 把 mDNS / SSDP 扫到的音箱登记成播放设备。
//
// 分三层：
//   - sink/raop、sink/dlna 只负责协议本身（发查询、解析响应）；
//   - 本包的 Scanner 把协议结果翻译成统一的 Speaker；
//   - Discovery 负责编排：并发扫描、去重、跳过不能用的设备、注册进 Manager。
//
// 之所以不放进 player 包：player 是纯编排层，不能反向依赖具体协议。
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
)

// DefaultInterval 默认扫描间隔，与 /settings/player-discovery 的默认值一致。
const DefaultInterval = 30 * time.Second

// MinInterval / MaxInterval 与配置端点的校验范围保持一致。
const (
	MinInterval = 10 * time.Second
	MaxInterval = 600 * time.Second
)

// Speaker 一台发现到的音箱（尚未入库）。
type Speaker struct {
	ID      string      `json:"id"`
	Kind    player.Kind `json:"kind"`
	Name    string      `json:"name"`
	Model   string      `json:"model"`
	Address string      `json:"address"`
	Caps    player.Caps `json:"caps"`
}

// Skipped 被跳过的设备（需要密码等）。
type Skipped struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// Result 一次扫描的结果。手动触发端点直接把它返回给前端。
type Result struct {
	Speakers []Speaker `json:"speakers"`
	// Added 本次新登记的设备 id。
	Added []string `json:"added"`
	// Updated 已在库、本次刷新了地址 / 能力的设备 id。
	Updated []string `json:"updated"`
	// Skipped 扫到但不能用的设备（目前只有"需要密码"）。
	Skipped []Skipped `json:"skipped,omitempty"`
	// Errors 单个协议的失败原因。一个协议挂了不影响另一个。
	Errors    []string  `json:"errors,omitempty"`
	ScannedAt time.Time `json:"scanned_at"`
}

// Scanner 一种协议的发现器。
type Scanner interface {
	Kind() player.Kind
	Scan(ctx context.Context) ([]Speaker, error)
}

// Registry Discovery 需要的最小管理能力（*player.Manager 满足）。
type Registry interface {
	GetPlayer(id string) (player.Player, error)
	RegisterPlayer(ctx context.Context, p player.Player) error
}

// Options 构造参数。
type Options struct {
	Registry Registry
	Scanners []Scanner
	Logger   *slog.Logger
	// ScanTimeout 单个协议的扫描上限。
	ScanTimeout time.Duration
	// Enabled / Protocols / Interval 初始配置。
	Enabled   bool
	Protocols []string
	Interval  time.Duration
}

// Discovery 周期扫描 + 手动扫描。
type Discovery struct {
	reg      Registry
	scanners []Scanner
	logger   *slog.Logger
	timeout  time.Duration

	mu        sync.RWMutex
	enabled   bool
	protocols map[player.Kind]bool
	interval  time.Duration
	last      Result
	lastAt    time.Time

	scanMu sync.Mutex // 扫描串行化：手动与周期不并发

	kick chan struct{}
	stop chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

// New 创建发现器。
func New(opts Options) *Discovery {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	timeout := opts.ScanTimeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	d := &Discovery{
		reg:       opts.Registry,
		scanners:  opts.Scanners,
		logger:    logger,
		timeout:   timeout,
		enabled:   opts.Enabled,
		protocols: protocolsSet(opts.Protocols),
		interval:  clampInterval(opts.Interval),
		kick:      make(chan struct{}, 1),
		stop:      make(chan struct{}),
	}
	return d
}

// ApplyConfig 应用 /settings/player-discovery 的新配置。
//
// 保存后立即唤醒扫描循环：关掉时马上停，改间隔时下一轮就用新值，
// 不用等旧的长间隔睡完。
func (d *Discovery) ApplyConfig(enabled bool, protocols []string, interval time.Duration) {
	d.mu.Lock()
	d.enabled = enabled
	d.protocols = protocolsSet(protocols)
	d.interval = clampInterval(interval)
	d.mu.Unlock()
	select {
	case d.kick <- struct{}{}:
	default:
	}
}

// Start 起后台循环。重复调用只生效一次。
func (d *Discovery) Start() {
	d.once.Do(func() {
		d.wg.Add(1)
		go d.loop()
	})
}

// Stop 停后台循环。手动扫描仍可用。
func (d *Discovery) Stop() {
	select {
	case <-d.stop:
	default:
		close(d.stop)
	}
	d.wg.Wait()
}

// Last 最近一次扫描结果。
func (d *Discovery) Last() (Result, time.Time) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.last, d.lastAt
}

// Enabled 当前是否周期扫描。
func (d *Discovery) Enabled() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.enabled
}

// Scan 立即扫描一次并登记设备。
//
// 不受 enabled 影响：手动触发（POST /players/discover）随时可用，
// 只受 protocols 限制——用户关掉的是"自动扫"，不是"能扫"。
func (d *Discovery) Scan(ctx context.Context) (Result, error) {
	d.scanMu.Lock()
	defer d.scanMu.Unlock()

	scanners := d.activeScanners()
	res := Result{ScannedAt: time.Now()}
	if len(scanners) == 0 || d.reg == nil {
		d.remember(res)
		return res, nil
	}

	type scanOut struct {
		kind player.Kind
		list []Speaker
		err  error
	}
	outs := make(chan scanOut, len(scanners))
	for _, s := range scanners {
		go func(s Scanner) {
			cctx, cancel := context.WithTimeout(ctx, d.timeout)
			defer cancel()
			list, err := s.Scan(cctx)
			outs <- scanOut{kind: s.Kind(), list: list, err: err}
		}(s)
	}
	seen := make(map[string]bool, len(scanners)*4)
	for range scanners {
		o := <-outs
		if o.err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", o.kind, o.err))
			continue
		}
		for _, sp := range o.list {
			if sp.ID == "" || seen[sp.ID] {
				continue
			}
			seen[sp.ID] = true
			res.Speakers = append(res.Speakers, sp)
		}
	}
	sort.Slice(res.Speakers, func(i, j int) bool {
		if res.Speakers[i].Name == res.Speakers[j].Name {
			return res.Speakers[i].ID < res.Speakers[j].ID
		}
		return res.Speakers[i].Name < res.Speakers[j].Name
	})

	for _, sp := range res.Speakers {
		if sp.Caps.NeedsPassword {
			// 需要密码的设备第一版直接跳过：RAOP 的密码是 FairPlay 认证，
			// 不是简单的 challenge-response，做了也放不出声。
			res.Skipped = append(res.Skipped, Skipped{ID: sp.ID, Name: sp.Name, Reason: "设备需要密码"})
			continue
		}
		_, err := d.reg.GetPlayer(sp.ID)
		existed := err == nil
		p := player.Player{
			ID:      sp.ID,
			Kind:    sp.Kind,
			Name:    sp.Name,
			Model:   sp.Model,
			Address: sp.Address,
			Caps:    sp.Caps,
		}
		if err := d.reg.RegisterPlayer(ctx, p); err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", sp.ID, err))
			continue
		}
		if existed {
			res.Updated = append(res.Updated, sp.ID)
		} else {
			res.Added = append(res.Added, sp.ID)
		}
	}
	d.remember(res)
	if len(res.Added) > 0 || len(res.Updated) > 0 {
		d.logger.Info("发现音箱", "added", len(res.Added), "updated", len(res.Updated), "skipped", len(res.Skipped))
	}
	return res, nil
}

func (d *Discovery) remember(res Result) {
	d.mu.Lock()
	d.last = res
	d.lastAt = res.ScannedAt
	d.mu.Unlock()
}

func (d *Discovery) activeScanners() []Scanner {
	d.mu.RLock()
	enabled := d.protocols
	d.mu.RUnlock()
	if len(enabled) == 0 {
		return nil
	}
	out := make([]Scanner, 0, len(d.scanners))
	for _, s := range d.scanners {
		if enabled[s.Kind()] {
			out = append(out, s)
		}
	}
	return out
}

func (d *Discovery) loop() {
	defer d.wg.Done()
	for {
		d.mu.RLock()
		enabled, interval := d.enabled, d.interval
		d.mu.RUnlock()

		if !enabled {
			select {
			case <-d.stop:
				return
			case <-d.kick:
				continue
			}
		}

		// 起服务后先扫一轮，别等 30 秒。
		ctx, cancel := context.WithTimeout(context.Background(), d.timeout*2)
		if _, err := d.Scan(ctx); err != nil {
			d.logger.Warn("周期发现失败", "err", err)
		}
		cancel()

		select {
		case <-d.stop:
			return
		case <-d.kick:
			continue
		case <-time.After(interval):
		}
	}
}

func protocolsSet(list []string) map[player.Kind]bool {
	out := make(map[player.Kind]bool, len(list))
	for _, p := range list {
		switch strings.ToLower(strings.TrimSpace(p)) {
		case "raop", "airplay":
			out[player.KindRAOP] = true
		case "dlna", "upnp":
			out[player.KindDLNA] = true
		}
	}
	return out
}

func clampInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultInterval
	}
	if d < MinInterval {
		return MinInterval
	}
	if d > MaxInterval {
		return MaxInterval
	}
	return d
}
