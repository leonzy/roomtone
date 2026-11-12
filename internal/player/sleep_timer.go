package player

import (
	"context"
	"errors"
	"time"
)

// SetSleepTimer 设置区域睡眠定时器，到点暂停该组。
//
// d <= 0 表示取消。重复设置会覆盖旧定时器。定时器只存在内存中：
// 服务重启后不恢复，避免半夜暂停用户刚恢复的播放。
func (m *Manager) SetSleepTimer(groupID string, d time.Duration) error {
	m.mu.Lock()
	if _, ok := m.groups[groupID]; !ok {
		m.mu.Unlock()
		return ErrGroupNotFound
	}
	m.cancelSleepTimerLocked(groupID)
	m.sleepGeneration++
	generation := m.sleepGeneration
	if d <= 0 {
		m.mu.Unlock()
		m.notifySongSleep(groupID, 0, generation)
		return nil
	}
	deadline := m.deps.Clock().Add(d)
	m.sleepTimers[groupID] = time.AfterFunc(d, func() { m.fireSleepTimer(groupID, deadline) })
	m.sleepDeadlines[groupID] = deadline
	m.mu.Unlock()
	m.notifySongSleep(groupID, 0, generation)
	return nil
}

// SleepTimer 返回区域睡眠定时器的截止时刻；没有活动定时器时 ok=false。
func (m *Manager) SleepTimer(groupID string) (deadline time.Time, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	deadline, ok = m.sleepDeadlines[groupID]
	return deadline, ok
}

// fireSleepTimer 定时器到点：先摘掉自己，再走引擎的 Pause 路径。
//
// Pause 内部会 persistState + emitState，所以客户端能收到 paused 的 state
// 事件；这里不重复广播，避免同一状态发两条。
func (m *Manager) fireSleepTimer(groupID string, deadline time.Time) {
	m.mu.Lock()
	current, ok := m.sleepDeadlines[groupID]
	if !ok || !current.Equal(deadline) {
		// 已被取消或替换：旧定时器的回调不能暂停新设置的睡眠。
		m.mu.Unlock()
		return
	}
	delete(m.sleepTimers, groupID)
	delete(m.sleepDeadlines, groupID)
	m.mu.Unlock()

	e := m.engine("group", groupID)
	if e == nil {
		return
	}
	if err := e.Pause(); err != nil {
		// 已经空闲时没有状态变化；暂停本身是幂等的，不当作故障。
		if !errors.Is(err, ErrInvalidTransition) {
			m.deps.Logger.Warn("sleep timer pause failed", "group", groupID, "err", err)
		}
		return
	}
	m.deps.Logger.Info("sleep timer fired", "group", groupID)
}

// cancelSleepTimerLocked 取消定时器；调用方必须持有 m.mu。
func (m *Manager) cancelSleepTimerLocked(groupID string) {
	delete(m.sleepSongs, groupID)
	if timer, ok := m.sleepTimers[groupID]; ok {
		timer.Stop()
	}
	delete(m.sleepTimers, groupID)
	delete(m.sleepDeadlines, groupID)
}

// cancelSleepTimer 取消定时器。
func (m *Manager) cancelSleepTimer(groupID string) {
	m.mu.Lock()
	m.cancelSleepTimerLocked(groupID)
	m.mu.Unlock()
}

// Song-count sleep belongs to a zone and counts natural completions, including
// the current song. It is deliberately not persisted across server restarts.
type songSleepTimer struct {
	remaining  int
	generation uint64
}

func (m *Manager) SetSleepAfterSongs(groupID string, songs int) error {
	if songs < 0 || songs > 1000 {
		return errors.New("songs must be between 0 and 1000")
	}
	m.mu.Lock()
	if _, ok := m.groups[groupID]; !ok {
		m.mu.Unlock()
		return ErrGroupNotFound
	}
	m.cancelSleepTimerLocked(groupID)
	m.sleepGeneration++
	generation := m.sleepGeneration
	if songs > 0 {
		m.sleepSongs[groupID] = &songSleepTimer{remaining: songs, generation: generation}
	}
	m.mu.Unlock()
	m.notifySongSleep(groupID, songs, generation)
	return nil
}

func (m *Manager) SleepSongs(groupID string) (int, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	plan, ok := m.sleepSongs[groupID]
	if !ok {
		return 0, false
	}
	return plan.remaining, true
}

// completeSleepSong runs outside the engine lock. The final completion waits
// for already-buffered PCM to be heard. Identity checks protect replacement or
// cancellation during that wait, so an old plan cannot pause a new session.
func (m *Manager) completeSleepSong(ctx context.Context, groupID string, finishAt time.Time) bool {
	m.mu.Lock()
	plan := m.sleepSongs[groupID]
	if ctx.Err() != nil || plan == nil || plan.remaining <= 0 {
		m.mu.Unlock()
		return false
	}
	plan.remaining--
	if plan.remaining > 0 {
		m.mu.Unlock()
		return false
	}
	m.mu.Unlock()
	if delay := finishAt.Sub(m.deps.Clock()); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sleepSongs[groupID] != plan {
		return false
	}
	delete(m.sleepSongs, groupID)
	return ctx.Err() == nil
}

// Local clients drive their own queues as well as delegated playback. Send the
// count to their native player to prevent eager auto-next, and accept their
// monotonic remaining-count reports only while the server engine is not running.
func (m *Manager) notifySongSleep(groupID string, songs int, generation uint64) {
	m.mu.RLock()
	var listeners []interface{ SetSleepAfterSongs(int, uint64) error }
	if g := m.groups[groupID]; g != nil {
		for _, member := range g.Members {
			if s, ok := m.sinks[member.PlayerID].(interface{ SetSleepAfterSongs(int, uint64) error }); ok {
				listeners = append(listeners, s)
			}
		}
	}
	m.mu.RUnlock()
	for _, s := range listeners {
		if err := s.SetSleepAfterSongs(songs, generation); err != nil {
			m.deps.Logger.Warn("send song sleep failed", "group", groupID, "err", err)
		}
	}
}

func (m *Manager) ReportSleepSongs(playerID string, generation uint64, remaining *int) {
	if generation == 0 || remaining == nil || *remaining < 0 {
		return
	}
	gid := m.GroupIDOf(playerID)
	e := m.engine("group", gid)
	if e == nil {
		return
	}
	e.mu.Lock()
	running := e.running
	e.mu.Unlock()
	if running {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.groups[gid]
	plan := m.sleepSongs[gid]
	if g == nil || len(g.Members) != 1 || plan == nil || plan.generation != generation || *remaining > plan.remaining {
		return
	}
	plan.remaining = *remaining
	if plan.remaining == 0 {
		delete(m.sleepSongs, gid)
	}
}

// RestoreSleepForPlayer re-sends the current plan after a Web/native reconnect.
// A zero plan clears only a timer the client previously received from the server.
func (m *Manager) RestoreSleepForPlayer(playerID string) {
	gid := m.GroupIDOf(playerID)
	m.mu.RLock()
	listener, ok := m.sinks[playerID].(interface{ SetSleepAfterSongs(int, uint64) error })
	songs := 0
	generation := m.sleepGeneration
	if plan := m.sleepSongs[gid]; plan != nil {
		songs = plan.remaining
		generation = plan.generation
	}
	m.mu.RUnlock()
	if ok {
		if err := listener.SetSleepAfterSongs(songs, generation); err != nil {
			m.deps.Logger.Warn("restore song sleep failed", "player", playerID, "err", err)
		}
	}
}
