package player

import (
	"context"
	"sync"
	"time"
)

// 派对同步的采样与干预参数。
//
// 与组内校正环（sync_loop.go）刻意分开：组内是"同一份解码源的帧偏移"，
// 可以丢帧/插静音，代价是 20ms；派对是"多个组各自解码"，只能整段 seek
// 或暂停等，代价是可听的断点，所以容忍值更宽、只在持续漂移时才动手。
const (
	// partySyncInterval 采样间隔，与组内校正环一致（对齐 LMS _CheckSync）。
	partySyncInterval = 950 * time.Millisecond
	// partyMaxFailures 连续校正失败次数，超过即判定 unaligned 并停止重试。
	partyMaxFailures = 3
	// partyCorrectTimeout 单次校正后的观察窗口：这段时间内仍超容忍值算一次失败。
	partyCorrectTimeout = 2 * time.Second
)

// partyMemberSync 单个成员组的同步状态。
type partyMemberSync struct {
	// failures 连续失败次数；成功后清零。
	failures int
	// unaligned 已放弃校正，只广播一次，不再重试。
	unaligned bool
	// correcting 上一次干预后还没被采样验证过。
	correcting bool
	// correctedAt 上一次干预的时刻，用于判定"校正后仍漂移"。
	correctedAt time.Time
	// pending 上次干预方向（-1 落后需 seek，+1 领先需等）。
	pending int
}

// partySyncState 一个派对的同步环。
type partySyncState struct {
	cancel  context.CancelFunc
	mu      sync.Mutex
	members map[string]*partyMemberSync
}

func newPartySyncState() *partySyncState {
	return &partySyncState{members: make(map[string]*partyMemberSync)}
}

func (s *partySyncState) member(groupID string) *partyMemberSync {
	s.mu.Lock()
	defer s.mu.Unlock()
	ms, ok := s.members[groupID]
	if !ok {
		ms = &partyMemberSync{}
		s.members[groupID] = ms
	}
	return ms
}

// startPartySync 启动派对同步环（幂等）。
func (m *Manager) startPartySync(partyID string) {
	m.mu.Lock()
	if m.partySync == nil {
		m.partySync = make(map[string]*partySyncState)
	}
	if _, ok := m.partySync[partyID]; ok {
		m.mu.Unlock()
		return
	}
	st := newPartySyncState()
	ctx, cancel := context.WithCancel(context.Background())
	st.cancel = cancel
	m.partySync[partyID] = st
	m.mu.Unlock()
	go m.runPartySync(ctx, partyID, st)
}

// stopPartySync 停止同步环并清理状态。
func (m *Manager) stopPartySync(partyID string) {
	m.mu.Lock()
	st, ok := m.partySync[partyID]
	delete(m.partySync, partyID)
	m.mu.Unlock()
	if ok && st.cancel != nil {
		st.cancel()
	}
}

func (m *Manager) syncState(partyID string) *partySyncState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.partySync[partyID]
}

// runPartySync 每 950ms 采样一次，超容忍值就校正。
func (m *Manager) runPartySync(ctx context.Context, partyID string, st *partySyncState) {
	t := time.NewTicker(partySyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.partySyncTick(ctx, partyID, st)
		}
	}
}

// partySyncTick 一次采样 + 校正。导出给测试用（CheckOnce）。
func (m *Manager) partySyncTick(ctx context.Context, partyID string, st *partySyncState) []PartyMember {
	p, err := m.GetParty(partyID)
	if err != nil {
		return nil
	}
	leader := m.engine("group", p.LeaderGroupID)
	if leader == nil {
		return nil
	}
	lst := leader.Status()
	// 主控不在播（暂停/停止/缓冲）时没有可比的位置，整轮跳过，
	// 否则会把"暂停"误判成"落后 10 秒"然后疯狂 seek。
	if lst.State != StatePlaying {
		return nil
	}
	tol := m.deps.PartyDriftToleranceMs
	out := make([]PartyMember, 0, len(p.Groups))
	for _, gid := range p.Groups {
		e := m.engine("group", gid)
		if e == nil {
			continue
		}
		mem := st.member(gid)
		st.mu.Lock()
		unaligned := mem.unaligned
		st.mu.Unlock()

		stt := e.Status()
		offsetMs := int((stt.Position - lst.Position) * 1000)
		pm := PartyMember{GroupID: gid, OffsetMs: offsetMs, State: string(stt.State), Unaligned: unaligned}
		out = append(out, pm)
		if unaligned || stt.State != StatePlaying {
			continue
		}
		drifting := offsetMs > tol || offsetMs < -tol
		if !drifting {
			m.partySyncConverged(partyID, gid, mem, offsetMs)
			continue
		}
		m.partySyncCorrect(ctx, partyID, gid, e, leader, mem, offsetMs)
	}
	return out
}

// partySyncConverged 已回到容忍范围内。
func (m *Manager) partySyncConverged(partyID, groupID string, mem *partyMemberSync, offsetMs int) {
	st := m.syncState(partyID)
	st.mu.Lock()
	wasCorrecting := mem.correcting
	mem.correcting = false
	mem.failures = 0
	mem.pending = 0
	st.mu.Unlock()
	if wasCorrecting && m.deps.Bus != nil {
		// corrected:true 只在"校正过并已回位"时发一次，前端据此把
		// "正在对齐…"收回到正常态。
		corrected := true
		m.deps.Bus.Publish(Event{
			Type:      EventGroupDrift,
			PartyID:   partyID,
			GroupID:   groupID,
			OffsetMs:  offsetMs,
			Corrected: &corrected,
		})
	}
}

// partySyncCorrect 对落后组 seek、对领先组暂停等待。
func (m *Manager) partySyncCorrect(ctx context.Context, partyID, groupID string, e, leader *Engine, mem *partyMemberSync, offsetMs int) {
	st := m.syncState(partyID)
	st.mu.Lock()
	// 上一次干预还没被验证：这是"校正无效"的一次失败。
	if mem.correcting && m.deps.Clock().Sub(mem.correctedAt) < partyCorrectTimeout {
		mem.failures++
	} else if mem.correcting {
		mem.failures++
		mem.correcting = false
	}
	failures := mem.failures
	st.mu.Unlock()

	if failures >= partyMaxFailures {
		m.partySyncGiveUp(partyID, groupID, mem, offsetMs)
		return
	}

	leaderPos := time.Duration(leader.Status().Position * float64(time.Second))
	if offsetMs < 0 {
		// 落后：直接 seek 到主控当前进度。派对里各组各自解码，帧级校正
		// 做不到，整段重开是唯一能把差距一次抹平的动作。
		if err := e.Seek(ctx, leaderPos); err != nil {
			m.deps.Logger.Warn("party seek failed", "party", partyID, "group", groupID, "err", err)
		}
	} else {
		// 领先：暂停 (offset) 之后恢复，让主控追上来，不产生跳音。
		wait := time.Duration(offsetMs) * time.Millisecond
		if err := e.Pause(); err != nil {
			m.deps.Logger.Warn("party pause failed", "party", partyID, "group", groupID, "err", err)
		} else {
			go func() {
				timer := time.NewTimer(wait)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return
				case <-timer.C:
					_ = e.Play(ctx)
				}
			}()
		}
	}

	st.mu.Lock()
	mem.correcting = true
	mem.correctedAt = m.deps.Clock()
	if offsetMs < 0 {
		mem.pending = -1
	} else {
		mem.pending = 1
	}
	st.mu.Unlock()

	if m.deps.Bus != nil {
		corrected := false
		m.deps.Bus.Publish(Event{
			Type:      EventGroupDrift,
			PartyID:   partyID,
			GroupID:   groupID,
			OffsetMs:  offsetMs,
			Corrected: &corrected,
		})
	}
}

// partySyncGiveUp 连续失败：判定 unaligned，停止重试，只广播一次。
func (m *Manager) partySyncGiveUp(partyID, groupID string, mem *partyMemberSync, offsetMs int) {
	st := m.syncState(partyID)
	st.mu.Lock()
	if mem.unaligned {
		st.mu.Unlock()
		return
	}
	mem.unaligned = true
	mem.correcting = false
	st.mu.Unlock()
	m.deps.Logger.Warn("party member unaligned", "party", partyID, "group", groupID, "offset_ms", offsetMs)
	if m.deps.Bus != nil {
		corrected := false
		unaligned := true
		m.deps.Bus.Publish(Event{
			Type:      EventGroupDrift,
			PartyID:   partyID,
			GroupID:   groupID,
			OffsetMs:  offsetMs,
			Corrected: &corrected,
			Unaligned: &unaligned,
		})
	}
}

// PartySyncOnce 手动跑一次同步采样（测试与运维排查用）。
func (m *Manager) PartySyncOnce(ctx context.Context, partyID string) []PartyMember {
	st := m.syncState(partyID)
	if st == nil {
		m.startPartySync(partyID)
		st = m.syncState(partyID)
	}
	if st == nil {
		return nil
	}
	return m.partySyncTick(ctx, partyID, st)
}
