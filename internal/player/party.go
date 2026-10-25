package player

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Party 派对：多个组同播同一份内容。
//
// 队列与状态挂在 party 引擎上（owner_kind='party'），各成员组**各自解码**
// ——共享一份解码器会让每台设备被迫接受同一 EQ / 同一延迟补偿，
// 而且一台掉线会拖垮全体。代价是派对里每个组各占 1 个解码槽位（§8.4）。
type Party struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	LeaderGroupID string   `json:"leader_group_id"`
	Groups        []string `json:"groups"`
}

// CreateParty 用给定组创建派对。
func (m *Manager) CreateParty(ctx context.Context, name string, groupIDs []string) (Party, error) {
	if !m.deps.PartyEnabled {
		return Party{}, ErrPartyDisabled
	}
	if len(groupIDs) == 0 {
		return Party{}, fmt.Errorf("group_ids 不能为空")
	}
	m.mu.Lock()
	for _, gid := range groupIDs {
		g, ok := m.groups[gid]
		if !ok {
			m.mu.Unlock()
			return Party{}, fmt.Errorf("%w: %s", ErrGroupNotFound, gid)
		}
		if g.PartyID != "" {
			m.mu.Unlock()
			return Party{}, fmt.Errorf("%w: 组 %s 已在派对 %s 中", ErrGroupInParty, gid, g.PartyID)
		}
	}
	pid := fmt.Sprintf("party-%d", time.Now().UnixNano())
	leader := groupIDs[0]
	p := &Party{ID: pid, Name: name, LeaderGroupID: leader, Groups: append([]string(nil), groupIDs...)}
	m.parties[pid] = p
	for _, gid := range groupIDs {
		m.groups[gid].PartyID = pid
	}
	snapshot := *p
	m.mu.Unlock()

	if m.deps.Store != nil {
		if err := m.deps.Store.SaveState(ctx, "party", pid, StateSnapshot{State: string(StateIdle), RepeatMode: string(RepeatOff)}); err != nil {
			m.deps.Logger.Warn("save party state failed", "party", pid, "err", err)
		}
		for _, gid := range groupIDs {
			g, err := m.GetGroup(gid)
			if err != nil {
				continue
			}
			_ = m.deps.Store.SaveGroup(ctx, g.Group)
		}
	}
	m.ensurePartyEngine(ctx, snapshot)
	m.startPartySync(pid)
	if m.deps.Bus != nil {
		for _, gid := range groupIDs {
			m.deps.Bus.Publish(Event{Type: EventPartyChanged, PartyID: pid, GroupID: gid, Action: "joined"})
		}
		m.deps.Bus.Publish(Event{Type: EventPartyChanged, PartyID: pid, Action: "created", Name: name})
	}
	return snapshot, nil
}

// JoinParty 把一个组加入派对，并从主控组当前进度接入。
func (m *Manager) JoinParty(ctx context.Context, partyID, groupID string) (Party, error) {
	m.mu.Lock()
	p, ok := m.parties[partyID]
	if !ok {
		m.mu.Unlock()
		return Party{}, ErrPartyNotFound
	}
	g, ok := m.groups[groupID]
	if !ok {
		m.mu.Unlock()
		return Party{}, ErrGroupNotFound
	}
	if g.PartyID != "" {
		m.mu.Unlock()
		return Party{}, ErrGroupInParty
	}
	g.PartyID = partyID
	p.Groups = append(p.Groups, groupID)
	snapshot := *p
	m.mu.Unlock()

	if m.deps.Bus != nil {
		m.deps.Bus.Publish(Event{Type: EventPartyChanged, PartyID: partyID, GroupID: groupID, Action: "joined"})
	}
	// 从主控组当前进度接入。
	if leader := m.engine("group", snapshot.LeaderGroupID); leader != nil {
		st := leader.Status()
		if st.SongID != 0 {
			if e := m.engine("group", groupID); e != nil {
				_ = e.Load(ctx, st.Queue.Items, st.Queue.Current, time.Duration(st.Position*float64(time.Second)))
			}
		}
	}
	return snapshot, nil
}

// LeaveParty 组退出派对。mode=stop 时该组停止出声，continue 时继续放当前内容。
func (m *Manager) LeaveParty(ctx context.Context, partyID, groupID, mode string) error {
	m.mu.Lock()
	p, ok := m.parties[partyID]
	if !ok {
		m.mu.Unlock()
		return ErrPartyNotFound
	}
	if g, ok := m.groups[groupID]; ok {
		g.PartyID = ""
	}
	groups := make([]string, 0, len(p.Groups))
	for _, gid := range p.Groups {
		if gid != groupID {
			groups = append(groups, gid)
		}
	}
	p.Groups = groups
	if p.LeaderGroupID == groupID && len(groups) > 0 {
		p.LeaderGroupID = groups[0]
	}
	empty := len(groups) == 0
	snapshot := *p
	if empty {
		delete(m.parties, partyID)
	}
	m.mu.Unlock()

	if mode == "stop" {
		if e := m.engine("group", groupID); e != nil {
			e.Stop()
		}
	}
	if m.deps.Bus != nil {
		m.deps.Bus.Publish(Event{Type: EventPartyChanged, PartyID: partyID, GroupID: groupID, Action: "left"})
	}
	if empty {
		m.stopPartySync(partyID)
		if e := m.engine("party", partyID); e != nil {
			e.Close()
			m.mu.Lock()
			delete(m.engines, "party:"+partyID)
			m.mu.Unlock()
		}
	} else if m.deps.Store != nil {
		_ = m.deps.Store.SaveGroup(ctx, Group{ID: groupID, Name: groupID})
	}
	_ = snapshot
	return nil
}

// EndParty 结束派对，所有成员组停止。
func (m *Manager) EndParty(ctx context.Context, partyID string) error {
	m.mu.Lock()
	p, ok := m.parties[partyID]
	if !ok {
		m.mu.Unlock()
		return ErrPartyNotFound
	}
	groups := append([]string(nil), p.Groups...)
	for _, gid := range groups {
		if g, ok := m.groups[gid]; ok {
			g.PartyID = ""
		}
	}
	delete(m.parties, partyID)
	m.mu.Unlock()

	for _, gid := range groups {
		if e := m.engine("group", gid); e != nil {
			e.Stop()
		}
	}
	m.stopPartySync(partyID)
	if e := m.engine("party", partyID); e != nil {
		e.Close()
		m.mu.Lock()
		delete(m.engines, "party:"+partyID)
		m.mu.Unlock()
	}
	if m.deps.Bus != nil {
		m.deps.Bus.Publish(Event{Type: EventPartyChanged, PartyID: partyID, Action: "ended"})
	}
	return nil
}

// GetParty 取派对。
func (m *Manager) GetParty(id string) (Party, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.parties[id]
	if !ok {
		return Party{}, ErrPartyNotFound
	}
	return *p, nil
}

// ListParties 列出派对。
func (m *Manager) ListParties() []Party {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Party, 0, len(m.parties))
	for _, p := range m.parties {
		out = append(out, *p)
	}
	return out
}

// PartyPlay 派对开播：所有成员组用同一队列、同一位置起播。
func (m *Manager) PartyPlay(ctx context.Context, partyID string, songIDs []int64, index int, position time.Duration) error {
	p, err := m.GetParty(partyID)
	if err != nil {
		return err
	}
	var firstErr error
	for _, gid := range p.Groups {
		e := m.engine("group", gid)
		if e == nil {
			continue
		}
		if err := e.Load(ctx, songIDs, index, position); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// PartyTransport 派对传输控制（暂停/播放/停止）。
func (m *Manager) PartyTransport(ctx context.Context, partyID, action string) error {
	p, err := m.GetParty(partyID)
	if err != nil {
		return err
	}
	var firstErr error
	for _, gid := range p.Groups {
		e := m.engine("group", gid)
		if e == nil {
			continue
		}
		var err error
		switch action {
		case "play":
			err = e.Play(ctx)
		case "pause":
			err = e.Pause()
		case "stop":
			e.Stop()
		default:
			err = fmt.Errorf("unsupported party action %q", action)
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// PartyStatus 派对状态：以主控组为准，附带各组偏移。
type PartyStatus struct {
	Party
	Leader  Status        `json:"leader_status"`
	Members []PartyMember `json:"members"`
}

// PartyMember 派对内单个组的同步情况。
type PartyMember struct {
	GroupID  string `json:"group_id"`
	OffsetMs int    `json:"offset_ms"`
	State    string `json:"state"`
	Drifting bool   `json:"drifting"`
	// Unaligned 连续校正失败，已停止重试；前端应显示"未对齐"而不是"正在对齐"。
	Unaligned bool `json:"unaligned"`
}

// Status 计算派对状态与漂移。
//
// 漂移 = 成员组位置 - 主控组位置。超过容忍值标记 drifting；
// 校正与 group_drift 事件都在 party_sync.go 的 950ms 采样里做，
// 这里只读，保证"查状态"不会产生副作用。
func (m *Manager) Status(ctx context.Context, partyID string) (PartyStatus, error) {
	p, err := m.GetParty(partyID)
	if err != nil {
		return PartyStatus{}, err
	}
	leader := m.engine("group", p.LeaderGroupID)
	if leader == nil {
		return PartyStatus{}, ErrGroupNotFound
	}
	lst := leader.Status()
	out := PartyStatus{Party: p, Leader: lst}
	tol := m.deps.PartyDriftToleranceMs
	for _, gid := range p.Groups {
		e := m.engine("group", gid)
		if e == nil {
			continue
		}
		st := e.Status()
		offset := int((st.Position - lst.Position) * 1000)
		drifting := offset > tol || offset < -tol
		pm := PartyMember{GroupID: gid, OffsetMs: offset, State: string(st.State), Drifting: drifting}
		// unaligned 由同步环维护：这里只读，不在这里发事件——
		// 事件由 party_sync.go 的 950ms 采样统一广播，避免一次 API 调用刷一串。
		if st := m.syncState(partyID); st != nil {
			st.mu.Lock()
			if ms, ok := st.members[gid]; ok {
				pm.Unaligned = ms.unaligned
			}
			st.mu.Unlock()
		}
		out.Members = append(out.Members, pm)
	}
	return out, nil
}

// ensurePartyEngine 创建派对引擎（持有共享队列与状态）。
func (m *Manager) ensurePartyEngine(ctx context.Context, p Party) {
	m.mu.Lock()
	key := "party:" + p.ID
	if _, ok := m.engines[key]; ok {
		m.mu.Unlock()
		return
	}
	e := NewEngine("party", p.ID, EngineDeps{
		Decoder:    m.deps.Decoder,
		Runners:    m.deps.Runners,
		Resolver:   m.deps.Resolver,
		Budget:     m.deps.Budget,
		Bus:        m.deps.Bus,
		Store:      m.deps.Store,
		Logger:     m.deps.Logger,
		Clock:      m.deps.Clock,
		Latency:    m.deps.Latency,
		ReplayGain: m.deps.ReplayGain,
	})
	m.engines[key] = e
	m.mu.Unlock()
}

// PartyEngine 派对引擎（队列归属）。
func (m *Manager) PartyEngine(partyID string) (*Engine, error) {
	if _, err := m.GetParty(partyID); err != nil {
		return nil, err
	}
	return m.engineFor("party", partyID)
}

// ErrPartyNotEnabled 兼容旧调用。
var ErrPartyNotEnabled = errors.New("party disabled")
