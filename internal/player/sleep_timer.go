package player

import (
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
	if old, ok := m.sleepTimers[groupID]; ok {
		old.Stop()
		delete(m.sleepTimers, groupID)
		delete(m.sleepDeadlines, groupID)
	}
	if d <= 0 {
		m.mu.Unlock()
		return nil
	}
	deadline := m.deps.Clock().Add(d)
	m.sleepTimers[groupID] = time.AfterFunc(d, func() { m.fireSleepTimer(groupID, deadline) })
	m.sleepDeadlines[groupID] = deadline
	m.mu.Unlock()
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
