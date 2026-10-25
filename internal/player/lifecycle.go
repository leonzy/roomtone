package player

import (
	"context"
	"fmt"
)

// 本文件是设备 / 组的"用户主动删除"路径（B8-04）。
//
// 与 UnregisterPlayer 的区别只在语义：那个是"设备从网络上消失了"，
// 这里是"用户在界面上点了删除"。清理动作完全共用，避免出现两套摘除
// 逻辑导致组里残留幽灵成员。两个边界必须成立：
//
//  1. 删最后一台设备：删完系统里没有设备也没有组，这是合法状态，
//     不能 panic，也不能把预算槽位漏着不还。
//  2. 删正在播放的组：先停引擎再摘成员，不能出现"分发器还在往已关闭
//     的 sink 写帧"，也不能留下占着解码槽位的孤儿引擎。

// DeletePlayer 永久删除设备。
//
// 设备在派对组里且是该组唯一成员时，先让这个组退出派对——否则派对
// 会留下一个指向已删除组的幽灵成员，校正环每轮都去读它的进度。
func (m *Manager) DeletePlayer(ctx context.Context, id string) error {
	if _, err := m.GetPlayer(id); err != nil {
		return err
	}
	if gid := m.GroupIDOf(id); gid != "" {
		if partyID, solo := m.partyOfSoloGroup(gid); solo {
			_ = m.LeaveParty(ctx, partyID, gid, "stop")
		}
	}
	if err := m.UnregisterPlayer(ctx, id); err != nil {
		return err
	}
	if m.deps.Bus != nil {
		m.deps.Bus.Publish(Event{Type: EventMemberChanged, PlayerID: id, Action: "deleted"})
	}
	return nil
}

// partyOfSoloGroup 返回组所在派对；solo 表示这个组只有一台设备。
func (m *Manager) partyOfSoloGroup(groupID string) (partyID string, solo bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	g, ok := m.groups[groupID]
	if !ok || g.PartyID == "" {
		return "", false
	}
	return g.PartyID, len(g.Members) <= 1
}

// DeleteGroup 删除组，成员各自回到单机组。
//
// 派对中的组不允许直接删：派对依赖组成员关系做进度对齐，静默拆掉会让
// 校正环对着不存在的组反复 seek。返回 ErrGroupInParty，由前端提示先退出。
func (m *Manager) DeleteGroup(ctx context.Context, groupID string) error {
	m.mu.RLock()
	g, ok := m.groups[groupID]
	if !ok {
		m.mu.RUnlock()
		return ErrGroupNotFound
	}
	if g.PartyID != "" {
		m.mu.RUnlock()
		return ErrGroupInParty
	}
	members := g.MemberIDs()
	m.mu.RUnlock()

	// 先停引擎：Stop 停分发协程，Close 释放解码预算。
	if e := m.engine("group", groupID); e != nil {
		e.Stop()
		e.Close()
	}
	m.mu.Lock()
	delete(m.engines, "group:"+groupID)
	delete(m.groups, groupID)
	m.retiredGroups[groupID] = true
	m.mu.Unlock()

	if m.deps.Store != nil {
		for _, id := range members {
			if err := m.deps.Store.DeleteMember(ctx, groupID, id); err != nil {
				m.deps.Logger.Warn("delete member failed", "group", groupID, "player", id, "err", err)
			}
		}
		if err := m.deps.Store.DeleteGroup(ctx, groupID); err != nil {
			m.deps.Logger.Warn("delete group failed", "id", groupID, "err", err)
		}
	}
	for _, id := range members {
		// 设备本身还在，必须回到自己的单机组，否则界面上就消失了。
		if err := m.ensureSoloGroup(ctx, id); err != nil {
			m.deps.Logger.Warn("restore solo group failed", "player", id, "err", err)
		}
	}
	if m.deps.Bus != nil {
		m.deps.Bus.Publish(Event{Type: EventGroupChanged, GroupID: groupID, Action: "deleted"})
	}
	return nil
}

// SetMaster 切换组的主控设备。
//
// 主控只决定音量基准与界面上的"跟着谁"，不参与时钟（时间轴锚在服务端
// 单调时钟上），所以播放中切换不需要重建解码器、不会断音。
func (m *Manager) SetMaster(ctx context.Context, groupID, playerID string) (GroupView, error) {
	m.mu.Lock()
	g, ok := m.groups[groupID]
	if !ok {
		m.mu.Unlock()
		return GroupView{}, ErrGroupNotFound
	}
	if !g.HasMember(playerID) {
		m.mu.Unlock()
		return GroupView{}, fmt.Errorf("%w: %s 不在该组中", ErrPlayerNotFound, playerID)
	}
	if g.MasterID == playerID {
		m.mu.Unlock()
		return m.GetGroup(groupID)
	}
	g.MasterID = playerID
	snapshot := *g
	m.mu.Unlock()

	if m.deps.Store != nil {
		if err := m.deps.Store.SaveGroup(ctx, snapshot); err != nil {
			m.deps.Logger.Warn("save group master failed", "group", groupID, "err", err)
		}
	}
	if m.deps.Bus != nil {
		m.deps.Bus.Publish(Event{Type: EventGroupChanged, GroupID: groupID, PlayerID: playerID, Action: "master_changed"})
	}
	return m.GetGroup(groupID)
}
