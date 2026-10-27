package player

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"songloft/internal/player/dsp"
	"songloft/internal/player/sink"
)

// 播放器 / 组的业务错误。handler 直接映射成 4xx，文案面向用户。
var (
	ErrPlayerNotFound  = errors.New("播放设备不存在")
	ErrPlayerOffline   = errors.New("播放设备离线")
	ErrGroupNotFound   = errors.New("同步组不存在")
	ErrGroupInParty    = errors.New("该组在派对中，请先退出派对")
	ErrGroupNotEmpty   = errors.New("目标组已有成员")
	ErrPartyNotFound   = errors.New("派对不存在")
	ErrPartyDisabled   = errors.New("派对模式已关闭")
	ErrDuplicateMember = errors.New("设备已在组内")
	// ErrBadMemberOrder 成员顺序请求不是该组的全排列。
	ErrBadMemberOrder = errors.New("成员顺序不合法")
	ErrUnsupported    = errors.New("设备不支持该操作")
	// ErrSyncTierMismatch 精确同步组不接受只能对齐起播的设备。
	ErrSyncTierMismatch = errors.New("设备不支持精确同步")
	ErrNotifyForbidden  = errors.New("事件来源不受信任")
	// ErrNothingPlaying 源组没有在播内容（转移 / 派对操作的先决条件不满足）。
	ErrNothingPlaying = errors.New("源组没有正在播放的内容")
	// ErrInvalidArgument 请求参数不合法，文案由调用点给出（见 invalidArg）。
	//
	// 单独建哨兵是因为这些错误以前用裸 fmt.Errorf，playbackStatus 认不出来就一律 500，
	// 客户端把“成员列表为空”当成服务端故障。
	ErrInvalidArgument = errors.New("参数错误")
	// ErrInvalidName 区域名称去空格后不是 1~40 个字符。
	ErrInvalidName = errors.New("区域名称不合法")
	// ErrInvalidIcon 区域图标不在内置白名单内。
	ErrInvalidIcon = errors.New("区域图标不合法")
)

// groupIcons 区域图标白名单（与前端内置图标一致，不开放上传）。
var groupIcons = map[string]bool{
	"speaker":   true,
	"bookshelf": true,
	"soundbar":  true,
	"headphone": true,
	"tv":        true,
	"phone":     true,
	"laptop":    true,
	"multiroom": true,
}

// validGroupIcon 空串表示使用默认图标，其余必须在白名单内。
func validGroupIcon(icon string) bool {
	return icon == "" || groupIcons[icon]
}

// ArgumentError 带用户文案的参数错误，errors.Is 命中 ErrInvalidArgument。
type ArgumentError struct{ Msg string }

func (e *ArgumentError) Error() string { return e.Msg }

// Is 让 errors.Is(err, ErrInvalidArgument) 成立，同时保留给用户看的原文案。
func (e *ArgumentError) Is(target error) bool { return target == ErrInvalidArgument }

// invalidArg 造一个参数错误（HTTP 400）。
func invalidArg(format string, args ...any) error {
	return &ArgumentError{Msg: fmt.Sprintf(format, args...)}
}

// Store 播放器域的持久化入口（由 database.PlaybackRepository 适配）。
type Store interface {
	StateStore
	ListPlayers(ctx context.Context) ([]Player, error)
	SavePlayer(ctx context.Context, p Player) error
	UpdatePlayerConfig(ctx context.Context, p Player) error
	UpdatePlayerVolume(ctx context.Context, p Player) error
	UpdatePlayerEQ(ctx context.Context, p Player) error
	DeletePlayer(ctx context.Context, id string) error

	ListGroups(ctx context.Context) ([]Group, error)
	SaveGroup(ctx context.Context, g Group) error
	UpdateGroupIdentity(ctx context.Context, id, name, icon string) error
	DeleteGroup(ctx context.Context, id string) error
	ListAllMembers(ctx context.Context) (map[string][]Member, error)
	SaveMember(ctx context.Context, groupID string, m Member) error
	UpdateMemberPosition(ctx context.Context, groupID, playerID string, position int) error
	DeleteMember(ctx context.Context, groupID, playerID string) error
}

// SinkBuilder 按设备类型造 sink。
//
// 返回值可能是 PushSink（raop / local）或 PullSink（dlna）；
// 只有 PushSink 会进引擎的分发列表，PullSink 由引擎在 Open 时下发地址。
type SinkBuilder interface {
	Build(ctx context.Context, p *Player) (sink.Sink, error)
}

// ManagerDeps 管理器依赖。
type ManagerDeps struct {
	Store        Store
	Bus          *EventBus
	Budget       *Budget
	Resolver     TrackResolver
	Decoder      DecoderFactory
	Runners      RunnerFactory
	Builder      SinkBuilder
	Logger       *slog.Logger
	Clock        func() time.Time
	Latency      time.Duration
	PartyEnabled bool
	// PartyDriftToleranceMs 派对漂移容忍（毫秒），默认 1500。
	PartyDriftToleranceMs int
	// ReplayGain 每首歌的增益来源；nil 表示不启用。
	ReplayGain ReplayGainSource
	// Crossfade 交叉淡化时长；返回 0 表示关闭（与 gapless 互斥）。
	Crossfade func() time.Duration
}

// Manager 设备 / 组 / 派对 / 引擎的编排者。
type Manager struct {
	mu      sync.RWMutex
	deps    ManagerDeps
	players map[string]*Player
	groups  map[string]*Group
	engines map[string]*Engine // key: "<kind>:<id>"
	sinks   map[string]sink.Sink
	parties map[string]*Party
	// eqChains 每台设备一条独立的均衡器链（按 playerID 缓存，改参数不重建）。
	eqChains map[string]*dsp.EQ
	// partySync 每个派对一个同步环（各自 950ms 采样）。
	partySync map[string]*partySyncState
	// metrics 运行指标（B8-03）：解码欠载、丢帧、偏差。
	metrics *Metrics
	// syncPending 刚入组 / 刚唤醒的设备及其"对齐中"截止时间。
	//
	// 只在 groupView 里读，过期即失效；不落库，重启后天然清空。
	syncPending map[string]time.Time
	// retiredGroups 被用户显式删除过的组 id，永不复用。
	//
	// 单机组的 id 是 "grp-<playerID>"，删掉一个组之后成员要各自回到单机组，
	// 如果允许复用，前端会看到"刚删掉的 id 又冒出来"，拿旧快照做 diff 的
	// 客户端会把新组当成旧组。所以删除过的 id 进墓碑表，重建时换带时间戳的 id。
	retiredGroups map[string]bool
	// sleepTimers 每个区域一个睡眠定时器（到点暂停该组）。
	//
	// 只存在内存里：睡眠定时器是"这一次睡前"的临时意图，服务重启后
	// 不应该继续在半夜暂停用户刚恢复的播放。
	sleepTimers map[string]*time.Timer
	// sleepDeadlines 与 sleepTimers 一一对应，供 GET 计算剩余时间，
	// 同时作为回调的代际标记（旧定时器到点后发现 deadline 不匹配就退出）。
	sleepDeadlines map[string]time.Time
}

// NewManager 创建管理器。
func NewManager(deps ManagerDeps) *Manager {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Clock == nil {
		deps.Clock = time.Now
	}
	if deps.Latency <= 0 {
		deps.Latency = 2 * time.Second
	}
	if deps.PartyDriftToleranceMs <= 0 {
		deps.PartyDriftToleranceMs = 1500
	}
	return &Manager{
		deps:      deps,
		players:   make(map[string]*Player),
		groups:    make(map[string]*Group),
		engines:   make(map[string]*Engine),
		sinks:     make(map[string]sink.Sink),
		parties:   make(map[string]*Party),
		eqChains:  make(map[string]*dsp.EQ),
		partySync: make(map[string]*partySyncState),

		metrics:        NewMetrics(),
		syncPending:    make(map[string]time.Time),
		retiredGroups:  make(map[string]bool),
		sleepTimers:    make(map[string]*time.Timer),
		sleepDeadlines: make(map[string]time.Time),
	}
}

// Bus 事件总线（handler 订阅用）。
func (m *Manager) Bus() *EventBus { return m.deps.Bus }

// Budget 解码预算。
func (m *Manager) Budget() *Budget { return m.deps.Budget }

// ListPlayers 列出设备（按名称排序，UI 稳定）。
func (m *Manager) ListPlayers() []Player {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Player, 0, len(m.players))
	for _, p := range m.players {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].ID < out[j].ID
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// GetPlayer 取设备。
func (m *Manager) GetPlayer(id string) (Player, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.players[id]
	if !ok {
		return Player{}, ErrPlayerNotFound
	}
	return *p, nil
}

// RegisterPlayer 注册（或刷新）一台设备。
//
// 首次注册时自动建一个只含自己的组——单机也走组逻辑，
// 队列/传输/同步只写一份代码（B1-06）。
func (m *Manager) RegisterPlayer(ctx context.Context, p Player) error {
	if p.ID == "" {
		return fmt.Errorf("player id is empty")
	}
	if p.Caps.MaxSampleRate <= 0 {
		p.Caps = defaultCaps()
	}
	m.mu.Lock()
	prev, existed := m.players[p.ID]
	rebuild := !existed || p.Kind == KindLocal || prev.Address != p.Address || !prev.Online || prev.Caps.PublicKey != p.Caps.PublicKey
	if existed {
		// 重连：旧 sink 绑的是已经关闭的 WS 连接，继续复用只会得到
		// "client closed"。丢掉重建，下面的 rebuildSinks 会用新连接重造。
		if stale, ok := m.sinks[p.ID]; ok && rebuild {
			delete(m.sinks, p.ID)
			_ = stale.Close()
		}
		// 保留用户设置（音量 / 音量模式 / 上限 / EQ / 接管行为 / 名称 / 密码）
		p.Volume = prev.Volume
		p.Muted = prev.Muted
		p.VolumeMode = prev.VolumeMode
		p.MaxVolume = prev.MaxVolume
		p.EQ = prev.EQ
		p.Takeover = prev.Takeover
		p.Password = prev.Password
		if prev.Name != "" {
			p.Name = prev.Name
		}
	} else {
		if p.Takeover == "" {
			p.Takeover = TakeoverPause
		}
		if p.Kind != KindLocal {
			// 只有自研客户端才谈"被切走后暂停"，音箱没有这个概念。
			p.Takeover = TakeoverPause
		}
		if p.VolumeMode == "" {
			p.VolumeMode = VolumeModeDevice
		}
		// 注册入口不区分"没传"与 0，0 视为未设置；用户要 0 走 PUT /players/{id}。
		if p.MaxVolume <= 0 {
			p.MaxVolume = DefaultMaxVolume
		}
	}
	// 防御存量脏数据：非法模式回退默认，上限越界回退 100。
	if !validVolumeMode(p.VolumeMode) {
		p.VolumeMode = VolumeModeDevice
	}
	if p.MaxVolume < 0 || p.MaxVolume > 100 {
		p.MaxVolume = DefaultMaxVolume
	}
	p.Online = true
	m.players[p.ID] = &p
	m.mu.Unlock()

	if m.deps.Store != nil {
		if err := m.deps.Store.SavePlayer(ctx, p); err != nil {
			m.deps.Logger.Warn("save player failed", "id", p.ID, "err", err)
		}
	}

	if !existed {
		if err := m.ensureSoloGroup(ctx, p.ID); err != nil {
			return err
		}
	}
	if m.deps.Bus != nil && !existed {
		m.deps.Bus.Publish(Event{Type: EventSpeakerSeen, PlayerID: p.ID, Name: p.Name, Kind: string(p.Kind)})
	}
	// 设备（重新）上线后必须重建所属组的 sink：服务重启时客户端还没连上，
	// 那时建不出 local sink；不在这里补一次，组里就永远没有控制型成员，
	// 起播会误报 distributor: no sinks。
	if gid := m.GroupIDOf(p.ID); gid != "" && rebuild {
		m.rebuildSinks(ctx, gid)
	}
	return nil
}

// UnregisterPlayer 删除设备：摘出所有组、清理孤儿组、释放 sink。
func (m *Manager) UnregisterPlayer(ctx context.Context, id string) error {
	m.mu.Lock()
	if _, ok := m.players[id]; !ok {
		m.mu.Unlock()
		return ErrPlayerNotFound
	}
	delete(m.players, id)
	if s, ok := m.sinks[id]; ok {
		_ = s.Close()
		delete(m.sinks, id)
	}
	affected := make([]string, 0, 1)
	for _, g := range m.groups {
		if g.HasMember(id) {
			affected = append(affected, g.ID)
		}
	}
	m.mu.Unlock()

	if m.deps.Store != nil {
		if err := m.deps.Store.DeletePlayer(ctx, id); err != nil && !errors.Is(err, ErrPlayerNotFound) {
			m.deps.Logger.Warn("delete player failed", "id", id, "err", err)
		}
	}
	for _, gid := range affected {
		if err := m.RemoveMember(ctx, gid, id, "stop"); err != nil && !errors.Is(err, ErrGroupNotFound) {
			m.deps.Logger.Warn("detach player from group failed", "group", gid, "player", id, "err", err)
		}
	}
	return nil
}

// SetOnline 更新在线状态并广播。
func (m *Manager) SetOnline(ctx context.Context, id string, online bool) {
	m.mu.Lock()
	p, ok := m.players[id]
	if !ok {
		m.mu.Unlock()
		return
	}
	if p.Online == online {
		m.mu.Unlock()
		return
	}
	p.Online = online
	groups := make([]string, 0, 1)
	for _, g := range m.groups {
		if g.HasMember(id) {
			groups = append(groups, g.ID)
		}
	}
	m.mu.Unlock()
	if m.deps.Store != nil {
		_ = m.deps.Store.SavePlayer(ctx, *p)
	}
	if m.deps.Bus != nil {
		action := "offline"
		if online {
			action = "online"
		}
		for _, gid := range groups {
			// 从属成员掉线**不改组状态**，只发 member_changed（§4.4）。
			m.deps.Bus.Publish(Event{Type: EventMemberChanged, GroupID: gid, PlayerID: id, Action: action})
		}
	}
}

// Reconnect 手动重连：丢掉缓存的 sink 重新握手，成功则标回在线。
//
// 音箱重启、换 IP、换固件后缓存的连接与能力都会过期，UI 的「重连」
// 按钮走这里。构建失败保持离线并返回 ErrPlayerOffline，由前端提示。
func (m *Manager) Reconnect(ctx context.Context, id string) (Player, error) {
	p, err := m.GetPlayer(id)
	if err != nil {
		return Player{}, err
	}
	m.mu.Lock()
	old, had := m.sinks[id]
	delete(m.sinks, id)
	m.mu.Unlock()
	if had && old != nil {
		_ = old.Close()
	}
	if _, ok := m.sinkFor(ctx, &p); !ok {
		m.SetOnline(ctx, id, false)
		return p, fmt.Errorf("%w: %s", ErrPlayerOffline, id)
	}
	m.SetOnline(ctx, id, true)
	if gid := m.GroupIDOf(id); gid != "" {
		// 组里正在播时让引擎拿新 sink，避免继续往死连接写帧。
		m.rebuildSinks(ctx, gid)
	}
	return m.GetPlayer(id)
}

// PlayerUpdate 设备可编辑配置；nil 字段表示不修改。
type PlayerUpdate struct {
	Name       *string
	Takeover   *TakeoverBehavior
	VolumeMode *string
	MaxVolume  *int
	// Password 设备密码（AirPlay 1 = HTTP Digest）。非 nil 更新（空串清空），
	// nil 不覆盖；不出 Player JSON，API 侧单独通道传入。
	Password *string
}

// UpdatePlayer 更新名称 / 接管行为 / 音量模式 / 音量上限。
//
// 音量上限调低时会把当前音量一并钳到新上限，否则 GET /players 会返回
// 一个超过用户刚设置的上限的值。
func (m *Manager) UpdatePlayer(ctx context.Context, id string, upd PlayerUpdate) (Player, error) {
	if upd.Name != nil {
		name := strings.TrimSpace(*upd.Name)
		if n := utf8.RuneCountInString(name); n < 1 || n > 40 {
			return Player{}, invalidArg("音箱名称长度需为 1~40 个字符")
		}
		upd.Name = &name
	}
	m.mu.Lock()
	p, ok := m.players[id]
	if !ok {
		m.mu.Unlock()
		return Player{}, ErrPlayerNotFound
	}
	oldName := p.Name
	if upd.Name != nil {
		p.Name = *upd.Name
	}
	if upd.Takeover != nil {
		takeover := *upd.Takeover
		if takeover != TakeoverPause && takeover != TakeoverFollow {
			m.mu.Unlock()
			return Player{}, invalidArg("takeover_behavior 只支持 pause / follow")
		}
		if p.Kind == KindLocal {
			p.Takeover = takeover
		}
	}
	if upd.VolumeMode != nil {
		mode := *upd.VolumeMode
		if !validVolumeMode(mode) {
			m.mu.Unlock()
			return Player{}, invalidArg("volume_mode 只支持 device / fixed / dsp")
		}
		p.VolumeMode = mode
	}
	if upd.Password != nil {
		p.Password = *upd.Password
	}
	volumeClamped := false
	if upd.MaxVolume != nil {
		maxVolume := *upd.MaxVolume
		if maxVolume < 0 || maxVolume > 100 {
			m.mu.Unlock()
			return Player{}, invalidArg("max_volume 必须在 0-100 之间")
		}
		p.MaxVolume = maxVolume
		if p.Volume > maxVolume {
			p.Volume = maxVolume
			volumeClamped = true
		}
	}
	// 密码更新后旧 sink 的握手凭据已失效，丢弃重建（下次起播用新密码）。
	if upd.Password != nil {
		if stale, ok := m.sinks[id]; ok {
			delete(m.sinks, id)
			_ = stale.Close()
		}
	}
	s := m.sinks[id]
	out := *p
	var soloGroup *Group
	if upd.Name != nil && out.Name != oldName {
		for _, g := range m.groups {
			if len(g.Members) == 1 && g.Members[0].PlayerID == id {
				copy := *g
				soloGroup = &copy
				break
			}
		}
	}
	m.mu.Unlock()
	if m.deps.Store != nil {
		_ = m.deps.Store.UpdatePlayerConfig(ctx, out)
		if volumeClamped {
			_ = m.deps.Store.UpdatePlayerVolume(ctx, out)
		}
	}
	if volumeClamped && s != nil {
		v := float64(out.Volume) / 100
		if out.Muted {
			v = 0
		}
		if err := s.SetVolume(v); err != nil {
			return out, err
		}
	}
	if soloGroup != nil {
		if _, err := m.RenameGroup(ctx, soloGroup.ID, out.Name, soloGroup.Icon); err != nil {
			return out, err
		}
	}
	if upd.Name != nil && m.deps.Bus != nil {
		m.deps.Bus.Publish(Event{Type: EventSpeakerSeen, PlayerID: id, Name: out.Name, Kind: string(out.Kind)})
	}
	return out, nil
}

// SetVolume 设置音量。音箱支持则直接下发，否则只改状态（本地播放器自己取）。
//
// volume_mode=fixed 时整次调用被忽略（返回当前值，不报错）：这类设备
// 的音量由外部控制，服务端不应覆盖。其余模式先钳到 max_volume。
func (m *Manager) SetVolume(ctx context.Context, id string, volume int, muted bool) (Player, error) {
	m.mu.Lock()
	p, ok := m.players[id]
	if !ok {
		m.mu.Unlock()
		return Player{}, ErrPlayerNotFound
	}
	if p.VolumeMode == VolumeModeFixed {
		out := *p
		m.mu.Unlock()
		m.deps.Logger.Debug("ignore volume change: player volume mode is fixed",
			"player_id", id, "requested", volume, "current", out.Volume)
		return out, nil
	}
	volume = clampVolume(volume)
	if volume > p.MaxVolume {
		volume = p.MaxVolume
	}
	p.Volume = volume
	p.Muted = muted
	s := m.sinks[id]
	out := *p
	m.mu.Unlock()
	if m.deps.Store != nil {
		_ = m.deps.Store.UpdatePlayerVolume(ctx, out)
	}
	if s != nil {
		v := float64(volume) / 100
		if muted {
			v = 0
		}
		if err := s.SetVolume(v); err != nil {
			return out, err
		}
	}
	return out, nil
}

// validVolumeMode 校验音量模式。
func validVolumeMode(mode string) bool {
	switch mode {
	case VolumeModeDevice, VolumeModeFixed, VolumeModeDSP:
		return true
	}
	return false
}

// SetEQ 更新某设备的 EQ，重算 preamp 并回填。
func (m *Manager) SetEQ(ctx context.Context, id string, eq EQProfile) (EQProfile, error) {
	m.mu.Lock()
	p, ok := m.players[id]
	if !ok {
		m.mu.Unlock()
		return EQProfile{}, ErrPlayerNotFound
	}
	p.EQ = eq
	out := *p
	groups := make([]string, 0, 1)
	for _, g := range m.groups {
		if g.HasMember(id) {
			groups = append(groups, g.ID)
		}
	}
	m.mu.Unlock()

	if m.deps.Store != nil {
		_ = m.deps.Store.UpdatePlayerEQ(ctx, out)
	}
	if m.deps.Bus != nil {
		enabled := eq.Enabled
		m.deps.Bus.Publish(Event{Type: EventEQChanged, PlayerID: id, Enabled: &enabled, Preset: eq.Preset})
	}
	// EQ 挂在分发的每路链上：重建目标即可，不需要重开解码器，
	// 播放中调整只会有 20ms 斜坡，不断音。
	for _, gid := range groups {
		m.rebuildSinks(ctx, gid)
	}
	return eq, nil
}

// ListGroups 列出组。
func (m *Manager) ListGroups() []GroupView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]GroupView, 0, len(m.groups))
	for _, g := range m.groups {
		out = append(out, m.groupViewLocked(g))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// GroupView 组 + 派生信息（同步等级、成员能力、信号路径）。
type GroupView struct {
	Group
	SyncTier   string       `json:"sync_tier"`
	MemberCaps []MemberCaps `json:"member_caps"`
	Status     Status       `json:"status"`
	PartyID    string       `json:"party_id,omitempty"`
	// SignalPath 只读诊断：源 → 解码 → DSP → 输出 → 设备。
	SignalPath SignalPath `json:"signal_path"`
}

// MemberCaps 成员能力摘要（前端用来提示"包含 DLNA 设备"）。
type MemberCaps struct {
	PlayerID string `json:"player_id"`
	Kind     string `json:"kind"`
	Online   bool   `json:"online"`
	SyncTier string `json:"sync_tier"`
	// Syncing 刚入组 / 刚唤醒，还没经过一个校正周期：前端显示"对齐中"。
	//
	// 校正环每 950ms 采样一次，正常一轮内收敛；这里给 3 个周期（约 3s）
	// 的窗口，超时自动消失，避免设备一直卡在"对齐中"。
	Syncing bool `json:"syncing"`
}

// syncAlignWindow 入组 / 唤醒后显示"对齐中"的窗口。
//
// 三个校正周期足够让一轮 Skip/Pad 落地；再长就会掩盖真正卡住的设备。
const syncAlignWindow = 3 * DefaultSyncInterval

// markSyncing 标记设备正在对齐（入组、唤醒、重挂都算）。
func (m *Manager) markSyncing(playerID string) {
	if playerID == "" {
		return
	}
	m.mu.Lock()
	m.syncPending[playerID] = m.deps.Clock().Add(syncAlignWindow)
	m.mu.Unlock()
}

// syncingLocked 设备是否仍在"对齐中"窗口内（调用方持锁）。
func (m *Manager) syncingLocked(playerID string) bool {
	deadline, ok := m.syncPending[playerID]
	if !ok {
		return false
	}
	if m.deps.Clock().After(deadline) {
		delete(m.syncPending, playerID)
		return false
	}
	return true
}

func (m *Manager) groupViewLocked(g *Group) GroupView {
	players := make([]*Player, 0, len(g.Members))
	caps := make([]MemberCaps, 0, len(g.Members))
	for _, mem := range g.Members {
		if p, ok := m.players[mem.PlayerID]; ok {
			players = append(players, p)
			caps = append(caps, MemberCaps{
				PlayerID: p.ID,
				Kind:     string(p.Kind),
				Online:   p.Online,
				SyncTier: p.Caps.SyncTier.String(),
				Syncing:  m.syncingLocked(p.ID),
			})
		}
	}
	tier := GroupSyncTier(players).String()
	view := GroupView{Group: *g, SyncTier: tier, MemberCaps: caps, PartyID: g.PartyID}
	var output Format
	source := "unknown"
	if e := m.engines["group:"+g.ID]; e != nil {
		view.Status = e.Status()
		output = e.Format()
		source = e.SourceFormat()
	}
	view.SignalPath = buildSignalPath(players, output, source, m.loudnessEnabledLocked())
	return view
}

// GetGroup 取组视图。
func (m *Manager) GetGroup(id string) (GroupView, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g, ok := m.groups[id]
	if !ok {
		return GroupView{}, ErrGroupNotFound
	}
	return m.groupViewLocked(g), nil
}

// ensureSoloGroup 为新设备建只含自己的组。
func (m *Manager) ensureSoloGroup(ctx context.Context, playerID string) error {
	m.mu.Lock()
	for _, g := range m.groups {
		if g.HasMember(playerID) {
			m.mu.Unlock()
			return nil
		}
	}
	p := m.players[playerID]
	gid := "grp-" + playerID
	if _, exists := m.groups[gid]; exists || m.retiredGroups[gid] {
		gid = fmt.Sprintf("grp-%s-%d", playerID, time.Now().UnixNano())
	}
	g := &Group{ID: gid, Name: p.Name, MasterID: playerID, Members: []Member{{PlayerID: playerID}}}
	m.groups[gid] = g
	snapshot := *g
	m.mu.Unlock()

	if m.deps.Store != nil {
		if err := m.deps.Store.SaveGroup(ctx, snapshot); err != nil {
			m.deps.Logger.Warn("save group failed", "id", gid, "err", err)
		}
		for _, mem := range snapshot.Members {
			if err := m.deps.Store.SaveMember(ctx, gid, mem); err != nil {
				m.deps.Logger.Warn("save member failed", "group", gid, "err", err)
			}
		}
	}
	m.ensureEngine(ctx, snapshot)
	return nil
}

// CreateGroup 新建同步组。
func (m *Manager) CreateGroup(ctx context.Context, name string, memberIDs []string, masterID string) (GroupView, error) {
	if len(memberIDs) == 0 {
		return GroupView{}, invalidArg("member_ids 不能为空")
	}
	m.mu.Lock()
	for _, id := range memberIDs {
		if _, ok := m.players[id]; !ok {
			m.mu.Unlock()
			return GroupView{}, fmt.Errorf("%w: %s", ErrPlayerNotFound, id)
		}
	}
	// 成员必须先从原组摘出来（一台设备同时只能属于一个组）。
	//
	// 走 RemoveMember 而不是直接 delete(m.groups)：后者只删内存不删库，
	// 重启后 Restore 会把旧组捞回来，同一台设备就同时挂在多个组里。
	// 顺带也保住了「多成员组只摘人、不整组拆掉」的语义。
	oldGroups := make(map[string]string, len(memberIDs))
	for _, id := range memberIDs {
		for _, g := range m.groups {
			if g.HasMember(id) {
				oldGroups[id] = g.ID
				break
			}
		}
	}
	m.mu.Unlock()

	for _, id := range memberIDs {
		gid, ok := oldGroups[id]
		if !ok {
			continue
		}
		if err := m.RemoveMember(ctx, gid, id, "stop"); err != nil && !errors.Is(err, ErrGroupNotFound) {
			return GroupView{}, err
		}
	}

	m.mu.Lock()
	gid := fmt.Sprintf("grp-%d", time.Now().UnixNano())
	master := masterID
	if master == "" || !contains(memberIDs, master) {
		master = memberIDs[0]
	}
	g := &Group{ID: gid, Name: name, MasterID: master}
	for _, id := range memberIDs {
		g.Members = append(g.Members, Member{PlayerID: id})
	}
	m.groups[gid] = g
	snapshot := *g
	m.mu.Unlock()

	if err := m.persistGroup(ctx, snapshot); err != nil {
		return GroupView{}, err
	}
	m.ensureEngine(ctx, snapshot)
	return m.GetGroup(gid)
}

// RenameGroup 更新区域名称与图标。
//
// 名称与图标是用户可见的区域身份，存服务端（player_groups.name/icon），
// 换设备不丢。name 去首尾空白后必须是 1~40 个字符；icon 为空表示使用
// 客户端默认图标，否则必须在 groupIcons 白名单内。
func (m *Manager) RenameGroup(ctx context.Context, id, name, icon string) (GroupView, error) {
	name = strings.TrimSpace(name)
	if n := utf8.RuneCountInString(name); n < 1 || n > 40 {
		return GroupView{}, fmt.Errorf("%w: 名称长度需为 1~40 个字符", ErrInvalidName)
	}
	if !validGroupIcon(icon) {
		return GroupView{}, fmt.Errorf("%w: %s", ErrInvalidIcon, icon)
	}

	m.mu.RLock()
	g, ok := m.groups[id]
	var soloPlayer *Player
	var oldSoloPlayer *Player
	if ok && len(g.Members) == 1 {
		if p := m.players[g.Members[0].PlayerID]; p != nil {
			copy := *p
			oldCopy := *p
			oldSoloPlayer = &oldCopy
			copy.Name = name
			soloPlayer = &copy
		}
	}
	m.mu.RUnlock()
	if !ok {
		return GroupView{}, ErrGroupNotFound
	}

	// 先落库再改内存：落库失败时不能留下“界面已改名、重启又变回去”的状态。
	if m.deps.Store != nil {
		if soloPlayer != nil {
			if err := m.deps.Store.UpdatePlayerConfig(ctx, *soloPlayer); err != nil {
				return GroupView{}, err
			}
		}
		if err := m.deps.Store.UpdateGroupIdentity(ctx, id, name, icon); err != nil {
			if oldSoloPlayer != nil {
				if rollbackErr := m.deps.Store.UpdatePlayerConfig(ctx, *oldSoloPlayer); rollbackErr != nil {
					m.deps.Logger.Error("rollback solo player name failed", "player", oldSoloPlayer.ID, "err", rollbackErr)
				}
			}
			return GroupView{}, err
		}
	}

	m.mu.Lock()
	if g, ok := m.groups[id]; ok {
		g.Name = name
		g.Icon = icon
	}
	if soloPlayer != nil {
		if p := m.players[soloPlayer.ID]; p != nil {
			p.Name = name
		}
	}
	m.mu.Unlock()

	if m.deps.Bus != nil {
		m.deps.Bus.Publish(Event{Type: EventGroupChanged, GroupID: id, Action: "renamed", Name: name})
		if soloPlayer != nil {
			m.deps.Bus.Publish(Event{Type: EventSpeakerSeen, PlayerID: soloPlayer.ID, Name: name, Kind: string(soloPlayer.Kind)})
		}
	}
	return m.GetGroup(id)
}

// purgeEmptyGroups 清理并组后留下的空组（含引擎与持久化记录）。
func (m *Manager) purgeEmptyGroups(ctx context.Context, ids []string) {
	for _, id := range ids {
		m.cancelSleepTimer(id)
		if e := m.engine("group", id); e != nil {
			e.Stop()
			e.Close()
			m.mu.Lock()
			delete(m.engines, "group:"+id)
			m.mu.Unlock()
		}
		if m.deps.Store != nil {
			if err := m.deps.Store.DeleteGroup(ctx, id); err != nil {
				m.deps.Logger.Warn("delete empty group failed", "id", id, "err", err)
			}
		}
	}
}

// AddMember 把设备加入组，从当前时间点接入。
func (m *Manager) AddMember(ctx context.Context, groupID, playerID string) (GroupView, error) {
	// Older clients edited only the solo zone name. Carry that identity into
	// the device before its solo group disappears during grouping.
	for _, id := range []string{groupID, m.GroupIDOf(playerID)} {
		view, err := m.GetGroup(id)
		if err == nil && len(view.Members) == 1 && strings.TrimSpace(view.Name) != "" {
			p, err := m.GetPlayer(view.Members[0].PlayerID)
			if err == nil && p.Name != view.Name {
				if _, err := m.RenameGroup(ctx, id, view.Name, view.Icon); err != nil {
					return GroupView{}, err
				}
			}
		}
	}
	m.mu.Lock()
	g, ok := m.groups[groupID]
	if !ok {
		m.mu.Unlock()
		return GroupView{}, ErrGroupNotFound
	}
	if _, ok := m.players[playerID]; !ok {
		m.mu.Unlock()
		return GroupView{}, ErrPlayerNotFound
	}
	if g.HasMember(playerID) {
		m.mu.Unlock()
		return GroupView{}, ErrDuplicateMember
	}
	// 精确同步组（RAOP / 本机）不能塞进只能对齐起播的设备（DLNA）：
	// 混进来整组都会降级成"起播对齐、之后随便漂"，用户却以为还在精确组。
	if joining, ok := m.players[playerID]; ok {
		if tier := m.groupTierLocked(g); tier == SyncExact && joining.Caps.SyncTier != SyncExact {
			m.mu.Unlock()
			return GroupView{}, fmt.Errorf("%w：%s 只能对齐起播，不能加入精确同步组",
				ErrSyncTierMismatch, joining.Name)
		}
	}
	// 从原组摘出。原组被摘空就必须删掉：设备并组后如果留着空壳，
	// 列表里会多一个没有成员的幽灵组，前端拿它当"可加入的组"渲染。
	emptied := make([]string, 0, 1)
	for _, other := range m.groups {
		if other.ID == groupID {
			continue
		}
		if other.HasMember(playerID) {
			other.Members = removeMember(other.Members, playerID)
			if len(other.Members) == 0 {
				emptied = append(emptied, other.ID)
				continue
			}
			if other.MasterID == playerID {
				other.MasterID = other.Members[0].PlayerID
			}
		}
	}
	for _, id := range emptied {
		delete(m.groups, id)
	}
	// 新成员排在最后：顺序决定"主控候选"，先入组的更靠前。
	newMember := Member{PlayerID: playerID, Position: len(g.Members)}
	g.Members = append(g.Members, newMember)
	snapshot := *g
	m.mu.Unlock()
	m.purgeEmptyGroups(ctx, emptied)

	if m.deps.Store != nil {
		_ = m.deps.Store.SaveMember(ctx, groupID, newMember)
	}
	m.rebuildSinks(ctx, groupID)
	// 刚入组的设备要经过一个校正周期才真正对齐，期间前端显示"对齐中"。
	m.markSyncing(playerID)
	if m.deps.Bus != nil {
		m.deps.Bus.Publish(Event{Type: EventMemberChanged, GroupID: groupID, PlayerID: playerID, Action: "join"})
	}
	return m.GetGroup(snapshot.ID)
}

// ReorderMembers 调整成员在组内的顺序。
//
// 顺序只影响展示与"主控候选"提示，不改主控（主控是显式的 master_id）：
// 拖错了不会突然换人出声。必须是同一组成员的全排列，少一个多一个都拒绝，
// 否则前端一次错误请求就能把成员从列表里"弄丢"。
func (m *Manager) ReorderMembers(ctx context.Context, groupID string, playerIDs []string) (GroupView, error) {
	m.mu.Lock()
	g, ok := m.groups[groupID]
	if !ok {
		m.mu.Unlock()
		return GroupView{}, ErrGroupNotFound
	}
	if len(playerIDs) != len(g.Members) {
		m.mu.Unlock()
		return GroupView{}, fmt.Errorf("%w: 需要 %d 个成员，收到 %d 个", ErrBadMemberOrder, len(g.Members), len(playerIDs))
	}
	byID := make(map[string]Member, len(g.Members))
	for _, mem := range g.Members {
		byID[mem.PlayerID] = mem
	}
	seen := make(map[string]bool, len(playerIDs))
	ordered := make([]Member, 0, len(playerIDs))
	for i, id := range playerIDs {
		mem, ok := byID[id]
		if !ok || seen[id] {
			m.mu.Unlock()
			return GroupView{}, fmt.Errorf("%w: %s", ErrBadMemberOrder, id)
		}
		seen[id] = true
		mem.Position = i
		ordered = append(ordered, mem)
	}
	g.Members = ordered
	m.mu.Unlock()

	if m.deps.Store != nil {
		for i, mem := range ordered {
			if err := m.deps.Store.UpdateMemberPosition(ctx, groupID, mem.PlayerID, i); err != nil {
				m.deps.Logger.Warn("update member position failed", "group", groupID, "player", mem.PlayerID, "err", err)
			}
		}
	}
	if m.deps.Bus != nil {
		m.deps.Bus.Publish(Event{Type: EventMemberChanged, GroupID: groupID, Action: "reorder"})
	}
	return m.GetGroup(groupID)
}

// tierRank 档位序（越小越好）：precise=0 < exact=1 < aligned=2。
// 组档位取 rank 最大（最差）成员——"任一成员不是 exact 整组就降档"的
// 既有语义在 precise 加入后仍成立，且兼容历史数字 0/1。
func tierRank(t SyncTier) int {
	switch t {
	case SyncPrecise:
		return 0
	case SyncExact:
		return 1
	default:
		return 2
	}
}

// groupTierLocked 组当前同步等级（调用方持锁）：取最差成员（rank 最大）。
// 首成员初始化——若以 exact 起步，全 precise 组将永远无法表达
// （precise 的 rank 更优，永不覆盖）。
func (m *Manager) groupTierLocked(g *Group) SyncTier {
	tier := SyncExact
	first := true
	for _, mem := range g.Members {
		p, ok := m.players[mem.PlayerID]
		if !ok {
			continue
		}
		if first || tierRank(p.Caps.SyncTier) > tierRank(tier) {
			tier = p.Caps.SyncTier
			first = false
		}
	}
	return tier
}

// UpdateMember 调整成员的延迟补偿与音量偏移。
//
// delay_ms 是"这台设备比主控晚出声多少"：吸顶喇叭走线长、蓝牙箱自带缓冲，
// 不给这个旋钮就只能整组迁就最慢的那台。volume_offset 让每台设备在自己
// 的音量基准上做微调（书架箱和吸顶喇叭灵敏度差 6dB 很常见）。
func (m *Manager) UpdateMember(ctx context.Context, groupID, playerID string, delayMs, volumeOffset int) (GroupView, error) {
	if delayMs < -2000 {
		delayMs = -2000
	}
	if delayMs > 2000 {
		delayMs = 2000
	}
	if volumeOffset < -100 {
		volumeOffset = -100
	}
	if volumeOffset > 100 {
		volumeOffset = 100
	}
	m.mu.Lock()
	g, ok := m.groups[groupID]
	if !ok {
		m.mu.Unlock()
		return GroupView{}, ErrGroupNotFound
	}
	found := false
	position := 0
	for i := range g.Members {
		if g.Members[i].PlayerID == playerID {
			g.Members[i].DelayMs = delayMs
			g.Members[i].VolumeOffset = volumeOffset
			position = g.Members[i].Position
			found = true
		}
	}
	m.mu.Unlock()
	if !found {
		return GroupView{}, fmt.Errorf("%w: %s", ErrPlayerNotFound, playerID)
	}
	if m.deps.Store != nil {
		_ = m.deps.Store.SaveMember(ctx, groupID, Member{PlayerID: playerID, DelayMs: delayMs, VolumeOffset: volumeOffset, Position: position})
	}
	// 延迟补偿要立刻生效：重建目标时把新 Delay 带进 sink.Target。
	m.rebuildSinks(ctx, groupID)
	if m.deps.Bus != nil {
		m.deps.Bus.Publish(Event{Type: EventMemberChanged, GroupID: groupID, PlayerID: playerID, Action: "update"})
	}
	return m.GetGroup(groupID)
}

// GroupVolumeMember 组内一台设备的实际音量。
type GroupVolumeMember struct {
	PlayerID     string `json:"player_id"`
	Volume       int    `json:"volume"`
	VolumeOffset int    `json:"volume_offset"`
	OK           bool   `json:"ok"`
	Error        string `json:"error,omitempty"`
}

// GroupVolume 组音量：base 是"基准"，每台实际值 = clamp(base + offset)。
type GroupVolume struct {
	Volume  int                 `json:"volume"`
	Muted   bool                `json:"muted"`
	Members []GroupVolumeMember `json:"members"`
}

// SetGroupVolume 同步下发组音量，各自叠加 volume_offset。
//
// 部分失败不回滚已成功的成员（音量不是事务性资源，回滚反而会让已经
// 调好的音箱再响一下），失败项放进 failed[]。
func (m *Manager) SetGroupVolume(ctx context.Context, groupID string, volume int, muted bool) (GroupVolume, error) {
	if volume < 0 {
		volume = 0
	}
	if volume > 100 {
		volume = 100
	}
	m.mu.RLock()
	g, ok := m.groups[groupID]
	if !ok {
		m.mu.RUnlock()
		return GroupVolume{}, ErrGroupNotFound
	}
	members := append([]Member(nil), g.Members...)
	m.mu.RUnlock()

	out := GroupVolume{Volume: volume, Muted: muted, Members: make([]GroupVolumeMember, 0, len(members))}
	for _, mem := range members {
		actual := clampVolume(volume + mem.VolumeOffset)
		item := GroupVolumeMember{PlayerID: mem.PlayerID, Volume: actual, VolumeOffset: mem.VolumeOffset}
		if got, err := m.SetVolume(ctx, mem.PlayerID, actual, muted); err != nil {
			item.Error = err.Error()
		} else {
			// fixed 模式或 max_volume 钳位后实际值可能不同于请求值，回显真实值。
			item.Volume = got.Volume
			item.OK = true
		}
		out.Members = append(out.Members, item)
	}
	return out, nil
}

// GroupVolume 读组音量：base 由主控反推（主控实际值 - 主控偏移）。
func (m *Manager) GroupVolumeOf(groupID string) (GroupVolume, error) {
	m.mu.RLock()
	g, ok := m.groups[groupID]
	if !ok {
		m.mu.RUnlock()
		return GroupVolume{}, ErrGroupNotFound
	}
	members := append([]Member(nil), g.Members...)
	masterID := g.MasterID
	m.mu.RUnlock()

	out := GroupVolume{Members: make([]GroupVolumeMember, 0, len(members))}
	baseSet := false
	for _, mem := range members {
		p, err := m.GetPlayer(mem.PlayerID)
		if err != nil {
			out.Members = append(out.Members, GroupVolumeMember{PlayerID: mem.PlayerID, VolumeOffset: mem.VolumeOffset, Error: err.Error()})
			continue
		}
		out.Members = append(out.Members, GroupVolumeMember{
			PlayerID: mem.PlayerID, Volume: p.Volume, VolumeOffset: mem.VolumeOffset, OK: true,
		})
		if mem.PlayerID == masterID {
			out.Volume = clampVolume(p.Volume - mem.VolumeOffset)
			out.Muted = p.Muted
			baseSet = true
		}
	}
	if !baseSet && len(out.Members) > 0 {
		out.Volume = out.Members[0].Volume
	}
	return out, nil
}

func clampVolume(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// RemoveMember 移出成员。mode=stop 时该设备停止出声，continue 时它自己继续放。
func (m *Manager) RemoveMember(ctx context.Context, groupID, playerID, mode string) error {
	m.mu.Lock()
	g, ok := m.groups[groupID]
	if !ok {
		m.mu.Unlock()
		return ErrGroupNotFound
	}
	if !g.HasMember(playerID) {
		m.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrPlayerNotFound, playerID)
	}
	g.Members = removeMember(g.Members, playerID)
	if s, ok := m.sinks[playerID]; ok {
		_ = s.Stop()
		if mode == "stop" {
			_ = s.Close()
			delete(m.sinks, playerID)
		}
	}
	if g.MasterID == playerID && len(g.Members) > 0 {
		g.MasterID = g.Members[0].PlayerID
	}
	empty := len(g.Members) == 0
	if empty {
		delete(m.groups, groupID)
		// 组没了，挂在它上面的睡眠定时器也必须一起取消，
		// 否则到点会去 Stop 一个已经不存在的引擎。
		m.cancelSleepTimerLocked(groupID)
	}
	m.mu.Unlock()

	if m.deps.Store != nil {
		_ = m.deps.Store.DeleteMember(ctx, groupID, playerID)
		if empty {
			_ = m.deps.Store.DeleteGroup(ctx, groupID)
		}
	}
	if empty {
		if e := m.engine("group", groupID); e != nil {
			e.Close()
			m.mu.Lock()
			delete(m.engines, "group:"+groupID)
			m.mu.Unlock()
		}
	} else {
		m.rebuildSinks(ctx, groupID)
	}
	if m.deps.Bus != nil {
		m.deps.Bus.Publish(Event{Type: EventMemberChanged, GroupID: groupID, PlayerID: playerID, Action: "leave"})
	}
	// 摘出来的设备回到自己的单机组，否则它在 UI 上会消失。
	if mode != "stop" {
		_ = m.ensureSoloGroup(ctx, playerID)
	}
	return nil
}

// TransferCompensation 转移时给目标组的前置补偿（源位置 + 这个值）。
const TransferCompensation = 800 * time.Millisecond

// Transfer 把播放从 fromGroup 转移到 toGroup（"播放跟着人走"）。
//
// onlyPlayerID 非空时表示"只在这台音箱上播"：把该设备从原组拆出来单独成组。
func (m *Manager) Transfer(ctx context.Context, fromGroupID, toGroupID, onlyPlayerID string) error {
	if onlyPlayerID != "" {
		view, err := m.GetGroup(fromGroupID)
		if err != nil {
			return err
		}
		if view.PartyID != "" {
			return ErrGroupInParty
		}
		if err := m.RemoveMember(ctx, fromGroupID, onlyPlayerID, "stop"); err != nil {
			return err
		}
		gid := fmt.Sprintf("grp-%d", time.Now().UnixNano())
		m.mu.Lock()
		p := m.players[onlyPlayerID]
		g := &Group{ID: gid, Name: p.Name, MasterID: onlyPlayerID, Members: []Member{{PlayerID: onlyPlayerID}}}
		m.groups[gid] = g
		snapshot := *g
		m.mu.Unlock()
		if err := m.persistGroup(ctx, snapshot); err != nil {
			return err
		}
		m.ensureEngine(ctx, snapshot)
		toGroupID = gid
	}

	from, err := m.engineFor("group", fromGroupID)
	if err != nil {
		return err
	}
	to, err := m.engineFor("group", toGroupID)
	if err != nil {
		return err
	}
	if from == to {
		return nil
	}
	st := from.Status()
	if st.SongID == 0 {
		return ErrNothingPlaying
	}
	from.Stop()
	songIDs := st.Queue.Items
	idx := st.Queue.Current
	if idx < 0 {
		idx = 0
	}
	// 转移补偿：从"停源"到"目标出声"之间必然有建连/起播的空档（实测约 800ms），
	// 不补这段就会听出一次明显的停顿。补偿从**源组的实时位置**算，不落库、
	// 不累加，所以连续转移不会越转越靠后。
	pos := time.Duration(st.Position*float64(time.Second)) + TransferCompensation
	if err := to.Load(ctx, songIDs, idx, pos); err != nil {
		return err
	}
	if m.deps.Bus != nil {
		m.deps.Bus.Publish(Event{Type: EventTargetChanged, GroupID: toGroupID, FromGroupID: fromGroupID, Reason: "transfer"})
	}
	return nil
}

// engine 取引擎（不存在返回 nil）。
func (m *Manager) engine(kind, id string) *Engine {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.engines[kind+":"+id]
}

// Engine 取组的引擎。
func (m *Manager) Engine(groupID string) (*Engine, error) {
	return m.engineFor("group", groupID)
}

func (m *Manager) engineFor(kind, id string) (*Engine, error) {
	m.mu.RLock()
	e := m.engines[kind+":"+id]
	m.mu.RUnlock()
	if e != nil {
		return e, nil
	}
	if kind == "group" {
		if _, ok := m.groups[id]; !ok {
			return nil, ErrGroupNotFound
		}
	}
	return nil, ErrGroupNotFound
}

// ensureEngine 为组创建引擎（幂等）。
func (m *Manager) ensureEngine(ctx context.Context, g Group) {
	m.mu.Lock()
	key := "group:" + g.ID
	if _, ok := m.engines[key]; ok {
		m.mu.Unlock()
		return
	}
	e := NewEngine("group", g.ID, EngineDeps{
		Decoder:    m.deps.Decoder,
		Runners:    m.deps.Runners,
		Resolver:   m.deps.Resolver,
		Budget:     m.deps.Budget,
		Bus:        m.deps.Bus,
		Store:      m.deps.Store,
		Logger:     m.deps.Logger,
		Clock:      m.deps.Clock,
		Latency:    m.deps.Latency,
		Metrics:    m.metrics,
		ReplayGain: m.deps.ReplayGain,
		Crossfade:  m.deps.Crossfade,
	})
	m.engines[key] = e
	m.mu.Unlock()
	m.rebuildSinks(ctx, g.ID)
}

// rebuildSinks 按组成员重建推送目标。
func (m *Manager) rebuildSinks(ctx context.Context, groupID string) {
	m.mu.RLock()
	g, ok := m.groups[groupID]
	if !ok {
		m.mu.RUnlock()
		return
	}
	players := make([]*Player, 0, len(g.Members))
	for _, mem := range g.Members {
		if p, ok := m.players[mem.PlayerID]; ok {
			players = append(players, p)
		}
	}
	e := m.engines["group:"+groupID]
	m.mu.RUnlock()
	if e == nil {
		return
	}
	format, err := NegotiateFormat(players)
	if err != nil {
		m.deps.Logger.Warn("negotiate format failed", "group", groupID, "err", err)
		return
	}
	e.SetFormat(format)
	targets := make([]sink.Target, 0, len(players))
	controls := make([]sink.Sink, 0, len(players))
	for _, p := range players {
		// 待机成员不参与分发，也不进同步校正环——否则每 950ms 都去"修"
		// 一台被用户主动关掉的音箱，日志里全是无法收敛的偏差。
		if p.Standby {
			continue
		}
		s, ok := m.sinkFor(ctx, p)
		if !ok {
			continue
		}
		// 控制型 sink（local 自拉流）不进分发列表：服务端拿不到它的 PCM，
		// 曲线只能下发给客户端自己执行，字段里写清 applied_by 防双重 EQ。
		m.syncControlEQ(s, p)
		ps, ok := s.(sink.PushSink)
		if !ok {
			// 但必须交给引擎：服务端要给它下发 load/play 指令，否则
			// 客户端永远收不到播放命令，组里只有它时还会误报 no sinks。
			controls = append(controls, s)
			continue
		}
		targets = append(targets, sink.Target{
			PlayerID: p.ID,
			Sink:     ps,
			EQ:       m.eqChainFor(p, format.SampleRate),
			Delay:    memberDelay(*g, p.ID),
		})
	}
	e.SetControls(controls)
	// 播放中增删成员走差异路径：新增的立刻接进时间轴，移除的立刻停。
	if err := e.SetTargets(ctx, targets); err != nil {
		m.deps.Logger.Warn("attach playback target failed", "group", groupID, "err", err)
	}
}

// eqSettingsFor 把设备 EQ 翻成下发结构（客户端与 WS 共用一份口径）。
func eqSettingsFor(p *Player, appliedBy string) sink.EQSettings {
	bands := dsp.NormalizeBands(p.EQ.Bands)
	return sink.EQSettings{
		Enabled:   !p.EQ.Flat(),
		Bands:     bands,
		Preamp:    p.EQ.Preamp,
		AppliedBy: appliedBy,
	}
}

// syncControlEQ 给控制型 sink 下发 EQ 状态。
//
// 这类设备（本机客户端）自己拉 /songs/{id}/stream，服务端不接触它的 PCM，
// 所以曲线必须由客户端执行；applied_by=client 让客户端不要等"服务端已处理"。
func (m *Manager) syncControlEQ(s sink.Sink, p *Player) {
	type eqSetter interface {
		SetEQ(sink.EQSettings) error
	}
	es, ok := s.(eqSetter)
	if !ok {
		return
	}
	if err := es.SetEQ(eqSettingsFor(p, sink.EQAppliedByClient)); err != nil {
		m.deps.Logger.Warn("push eq to client failed", "player", p.ID, "err", err)
	}
}

// eqChainFor 取（或建）某设备的 EQ 链。
//
// 每台设备一条独立链：同一组里书架箱和吸顶喇叭的补偿完全不同，
// 共享一条滤镜链只能有一个声音。采样率变化（组协商结果变了）时重建。
func (m *Manager) eqChainFor(p *Player, rate int) sink.SampleProcessor {
	m.mu.Lock()
	defer m.mu.Unlock()
	if chain, ok := m.eqChains[p.ID]; ok {
		chain.SetBands(p.EQ.Bands, p.EQ.Preamp)
		return chain
	}
	chain := dsp.NewEQ(rate, 2, p.EQ.Bands, p.EQ.Preamp)
	m.eqChains[p.ID] = chain
	return chain
}

// memberDelay 组成员延迟补偿。
func memberDelay(g Group, playerID string) time.Duration {
	for _, mem := range g.Members {
		if mem.PlayerID == playerID {
			return time.Duration(mem.DelayMs) * time.Millisecond
		}
	}
	return 0
}

// sinkFor 取（或建）某设备的 sink。PullSink（DLNA）不进分发列表，
// 但要保留实例供 Open 时下发地址。
func (m *Manager) sinkFor(ctx context.Context, p *Player) (sink.Sink, bool) {
	m.mu.RLock()
	s, ok := m.sinks[p.ID]
	m.mu.RUnlock()
	if ok {
		return s, true
	}
	if m.deps.Builder == nil {
		return nil, false
	}
	built, err := m.deps.Builder.Build(ctx, p)
	if err != nil {
		m.deps.Logger.Warn("build sink failed", "player", p.ID, "err", err)
		return nil, false
	}
	if p.Kind == KindRAOP || p.Kind == KindRAAT {
		volume := float64(min(p.Volume, p.MaxVolume)) / 100
		if p.Muted {
			volume = 0
		}
		_ = built.SetVolume(volume)
	}
	m.mu.Lock()
	m.sinks[p.ID] = built
	m.mu.Unlock()
	return built, true
}

// SinkOf 取某设备当前绑定的 sink（不存在返回 false）。
func (m *Manager) SinkOf(playerID string) (sink.Sink, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sinks[playerID]
	return s, ok
}

// Restore 启动时从库恢复设备、组、成员与队列。
func (m *Manager) Restore(ctx context.Context) error {
	if m.deps.Store == nil {
		return nil
	}
	players, err := m.deps.Store.ListPlayers(ctx)
	if err != nil {
		return err
	}
	m.mu.Lock()
	for i := range players {
		p := players[i]
		p.Online = false // 重启后一律先标离线，等发现/重连再上线
		m.players[p.ID] = &p
	}
	m.mu.Unlock()

	groups, err := m.deps.Store.ListGroups(ctx)
	if err != nil {
		return err
	}
	members, err := m.deps.Store.ListAllMembers(ctx)
	if err != nil {
		return err
	}
	for _, g := range groups {
		g.Members = members[g.ID]
		if len(g.Members) == 0 {
			continue
		}
		m.mu.Lock()
		m.groups[g.ID] = &g
		m.mu.Unlock()
		if len(g.Members) == 1 && strings.TrimSpace(g.Name) != "" {
			p, err := m.GetPlayer(g.Members[0].PlayerID)
			if err == nil && p.Name != g.Name {
				if _, err := m.RenameGroup(ctx, g.ID, g.Name, g.Icon); err != nil {
					return err
				}
			}
		}
		m.ensureEngine(ctx, g)
		if e := m.engine("group", g.ID); e != nil {
			if err := e.Restore(ctx); err != nil {
				m.deps.Logger.Warn("restore engine failed", "group", g.ID, "err", err)
			}
		}
	}
	return nil
}

func (m *Manager) persistGroup(ctx context.Context, g Group) error {
	if m.deps.Store == nil {
		return nil
	}
	if err := m.deps.Store.SaveGroup(ctx, g); err != nil {
		return err
	}
	for _, mem := range g.Members {
		if err := m.deps.Store.SaveMember(ctx, g.ID, mem); err != nil {
			return err
		}
	}
	return nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func removeMember(list []Member, id string) []Member {
	out := make([]Member, 0, len(list))
	for _, m := range list {
		if m.PlayerID != id {
			out = append(out, m)
		}
	}
	return out
}

// GroupIDOf 设备所在组的 id（没有则空）。
func (m *Manager) GroupIDOf(playerID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, g := range m.groups {
		if g.HasMember(playerID) {
			return g.ID
		}
	}
	return ""
}

// DeviceControl 设备侧反控入口（DACP / RAAT subscribe_transport 共用）。
//
// cmd: play / pause / stop / nextitem / previtem / preventplayback /
// setvolume（value 为 0..1）/ seek（value 为秒）。未知命令返回错误。
func (m *Manager) DeviceControl(ctx context.Context, playerID, cmd string, value float64) error {
	gid := m.GroupIDOf(playerID)
	e := m.engine("group", gid)
	if e == nil {
		return ErrPlayerNotFound
	}
	switch cmd {
	case "play":
		return e.Play(ctx)
	case "pause", "preventplayback":
		return e.Pause()
	case "stop":
		e.Stop()
		return nil
	case "nextitem":
		return e.Next(ctx, false)
	case "previtem":
		return e.Prev(ctx)
	case "setvolume":
		_, err := m.SetVolume(ctx, playerID, int(value*100), false)
		return err
	case "seek":
		return e.Seek(ctx, time.Duration(value)*time.Second)
	default:
		return fmt.Errorf("device control: unknown cmd %q", cmd)
	}
}

// MirrorReport 把某台设备的上报镜像到它所在组的引擎。
//
// 设备不在任何组里时直接忽略：单机播放不需要服务端记账。
func (m *Manager) MirrorReport(playerID string, songID int64, state string, position time.Duration) {
	gid := m.GroupIDOf(playerID)
	if gid == "" {
		return
	}
	if e := m.engine("group", gid); e != nil {
		e.MirrorReport(songID, state, position)
	}
}

// DescribePlayer 供日志/诊断的一行摘要。
func DescribePlayer(p Player) string {
	return fmt.Sprintf("%s(%s,%s)", p.Name, p.Kind, strings.Join([]string{string(p.ID)}, ""))
}

// HandleSinkNotify 把设备推来的事件转给对应 sink。
//
// remoteHost 是请求来源 IP：只有设备自己上报的事件才被接受，否则
// 任何能访问局域网的人都能伪造"已停止播放"。player 地址为空时直接拒绝。
func (m *Manager) HandleSinkNotify(remoteHost, playerID string, body []byte) error {
	p, err := m.GetPlayer(playerID)
	if err != nil {
		return err
	}
	host := p.Address
	if u, err := url.Parse(host); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" || host != remoteHost {
		return ErrNotifyForbidden
	}
	s, ok := m.SinkOf(playerID)
	if !ok {
		return ErrPlayerOffline
	}
	n, ok := s.(interface{ HandleNotify([]byte) })
	if !ok {
		return ErrNotifyForbidden
	}
	n.HandleNotify(body)
	return nil
}
