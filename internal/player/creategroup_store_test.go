package player_test

import (
	"context"
	"testing"
	"time"
)

// 新建区域把设备从原组摘出来时，必须同步删库。
//
// 以前 CreateGroup 只 delete(m.groups) 不碰 store：内存里旧组没了，库里还在。
// 重启后 Restore 把旧组捞回来，同一台设备就同时挂在多个组里（界面上出现
// 多个同名区域，播放归属也说不清）。
func TestCreateGroupDetachesMemberFromStore(t *testing.T) {
	store := newMemStore()
	m, _, _ := newTestManagerWithStore(t, store, time.Now, "p1")
	ctx := context.Background()

	oldGID := m.GroupIDOf("p1")
	if oldGID == "" {
		t.Fatal("注册后应有单机组")
	}
	if _, err := m.CreateGroup(ctx, "客厅", []string{"p1"}, ""); err != nil {
		t.Fatalf("create group: %v", err)
	}

	// 模拟重启：同一个 store 上再起一个管理器。
	m2, _, _ := newTestManagerWithStore(t, store, time.Now)
	if err := m2.Restore(ctx); err != nil {
		t.Fatalf("restore: %v", err)
	}
	groups := m2.ListGroups()
	if len(groups) != 1 {
		t.Fatalf("重启后应只剩 1 个组，实际 %d 个：%+v", len(groups), groups)
	}
	if groups[0].Name != "客厅" {
		t.Fatalf("重启后应保留新组「客厅」，实际 %q", groups[0].Name)
	}
}

// 多成员组里摘走一台设备，不能把整组拆掉。
func TestCreateGroupKeepsOtherMembers(t *testing.T) {
	m, _, _ := newTestManager(t, "p1", "p2")
	ctx := context.Background()

	gid := m.GroupIDOf("p1")
	if _, err := m.AddMember(ctx, gid, "p2"); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if _, err := m.CreateGroup(ctx, "书房", []string{"p1"}, ""); err != nil {
		t.Fatalf("create group: %v", err)
	}

	old, err := m.GetGroup(gid)
	if err != nil {
		t.Fatalf("原组不应被拆掉：%v", err)
	}
	if len(old.Members) != 1 || old.Members[0].PlayerID != "p2" {
		t.Fatalf("原组应只剩 p2，实际 %+v", old.Members)
	}
	if got := m.GroupIDOf("p1"); got == gid {
		t.Fatalf("p1 应已离开原组")
	}
}
