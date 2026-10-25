package player

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

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
	ErrUnsupported     = errors.New("设备不支持该操作")
	ErrNotifyForbidden = errors.New("事件来源不受信任")
)

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
	DeleteGroup(ctx context.Context, id string) error
	ListAllMembers(ctx context.Context) (map[string][]Member, error)
	SaveMember(ctx context.Context, groupID string, m Member) error
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
		deps:    deps,
		players: make(map[string]*Player),
		groups:  make(map[string]*Group),
		engines: make(map[string]*Engine),
		sinks:   make(map[string]sink.Sink),
		parties: make(map[string]*Party),
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
	if existed {
		// 保留用户设置（音量 / EQ / 接管行为 / 名称）
		p.Volume = prev.Volume
		p.Muted = prev.Muted
		p.EQ = prev.EQ
		p.Takeover = prev.Takeover
		if p.Name == "" {
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

// UpdatePlayer 更新名称 / 接管行为 / 音量 / EQ。
func (m *Manager) UpdatePlayer(ctx context.Context, id, name string, takeover TakeoverBehavior) (Player, error) {
	m.mu.Lock()
	p, ok := m.players[id]
	if !ok {
		m.mu.Unlock()
		return Player{}, ErrPlayerNotFound
	}
	if name != "" {
		p.Name = name
	}
	if takeover != "" {
		if takeover != TakeoverPause && takeover != TakeoverFollow {
			m.mu.Unlock()
			return Player{}, fmt.Errorf("invalid takeover_behavior %q", takeover)
		}
		if p.Kind == KindLocal {
			p.Takeover = takeover
		}
	}
	out := *p
	m.mu.Unlock()
	if m.deps.Store != nil {
		_ = m.deps.Store.UpdatePlayerConfig(ctx, out)
	}
	return out, nil
}

// SetVolume 设置音量。音箱支持则直接下发，否则只改状态（本地播放器自己取）。
func (m *Manager) SetVolume(ctx context.Context, id string, volume int, muted bool) (Player, error) {
	if volume < 0 {
		volume = 0
	}
	if volume > 100 {
		volume = 100
	}
	m.mu.Lock()
	p, ok := m.players[id]
	if !ok {
		m.mu.Unlock()
		return Player{}, ErrPlayerNotFound
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
	// EQ 变了要重开解码器才生效。
	for _, gid := range groups {
		if e := m.engine("group", gid); e != nil {
			e.SetEQ(m.groupEQ(gid))
		}
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

// GroupView 组 + 派生信息（同步等级、成员能力）。
type GroupView struct {
	Group
	SyncTier   string       `json:"sync_tier"`
	MemberCaps []MemberCaps `json:"member_caps"`
	Status     Status       `json:"status"`
	PartyID    string       `json:"party_id,omitempty"`
}

// MemberCaps 成员能力摘要（前端用来提示"包含 DLNA 设备"）。
type MemberCaps struct {
	PlayerID string `json:"player_id"`
	Kind     string `json:"kind"`
	Online   bool   `json:"online"`
	SyncTier string `json:"sync_tier"`
}

func (m *Manager) groupViewLocked(g *Group) GroupView {
	players := make([]*Player, 0, len(g.Members))
	caps := make([]MemberCaps, 0, len(g.Members))
	for _, mem := range g.Members {
		if p, ok := m.players[mem.PlayerID]; ok {
			players = append(players, p)
			caps = append(caps, MemberCaps{PlayerID: p.ID, Kind: string(p.Kind), Online: p.Online, SyncTier: p.Caps.SyncTier.String()})
		}
	}
	tier := GroupSyncTier(players).String()
	view := GroupView{Group: *g, SyncTier: tier, MemberCaps: caps, PartyID: g.PartyID}
	if e := m.engines["group:"+g.ID]; e != nil {
		view.Status = e.Status()
	}
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
	if _, exists := m.groups[gid]; exists {
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
		return GroupView{}, fmt.Errorf("member_ids 不能为空")
	}
	m.mu.Lock()
	for _, id := range memberIDs {
		if _, ok := m.players[id]; !ok {
			m.mu.Unlock()
			return GroupView{}, fmt.Errorf("%w: %s", ErrPlayerNotFound, id)
		}
	}
	// 成员必须先从原组摘出来（一台设备同时只能属于一个组）。
	for _, id := range memberIDs {
		for _, g := range m.groups {
			if g.HasMember(id) {
				delete(m.groups, g.ID)
				break
			}
		}
	}
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

// AddMember 把设备加入组，从当前时间点接入。
func (m *Manager) AddMember(ctx context.Context, groupID, playerID string) (GroupView, error) {
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
	// 从原组摘出。
	for _, other := range m.groups {
		if other.ID == groupID {
			continue
		}
		if other.HasMember(playerID) {
			other.Members = removeMember(other.Members, playerID)
		}
	}
	g.Members = append(g.Members, Member{PlayerID: playerID})
	snapshot := *g
	m.mu.Unlock()

	if m.deps.Store != nil {
		_ = m.deps.Store.SaveMember(ctx, groupID, Member{PlayerID: playerID})
	}
	m.rebuildSinks(ctx, groupID)
	if m.deps.Bus != nil {
		m.deps.Bus.Publish(Event{Type: EventMemberChanged, GroupID: groupID, PlayerID: playerID, Action: "join"})
	}
	return m.GetGroup(snapshot.ID)
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
		return fmt.Errorf("源组没有正在播放的内容")
	}
	from.Stop()
	songIDs := st.Queue.Items
	idx := st.Queue.Current
	if idx < 0 {
		idx = 0
	}
	if err := to.Load(ctx, songIDs, idx, time.Duration(st.Position*float64(time.Second))); err != nil {
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
		Decoder:  m.deps.Decoder,
		Runners:  m.deps.Runners,
		Resolver: m.deps.Resolver,
		Budget:   m.deps.Budget,
		Bus:      m.deps.Bus,
		Store:    m.deps.Store,
		Logger:   m.deps.Logger,
		Clock:    m.deps.Clock,
		Latency:  m.deps.Latency,
	})
	m.engines[key] = e
	m.mu.Unlock()
	e.SetEQ(m.groupEQ(g.ID))
	m.rebuildSinks(ctx, g.ID)
}

// groupEQ 组 EQ 取主控设备的配置（第一版：一台音箱一个 EQ，组内以主控为准）。
func (m *Manager) groupEQ(groupID string) EQProfile {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g, ok := m.groups[groupID]
	if !ok {
		return EQProfile{}
	}
	if p, ok := m.players[g.MasterID]; ok {
		return p.EQ
	}
	return EQProfile{}
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
	push := make([]sink.PushSink, 0, len(players))
	for _, p := range players {
		s, ok := m.sinkFor(ctx, p)
		if !ok {
			continue
		}
		if ps, ok := s.(sink.PushSink); ok {
			push = append(push, ps)
		}
	}
	e.SetSinks(push)
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
