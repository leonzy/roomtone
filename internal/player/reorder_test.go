package player_test

import (
	"context"
	"errors"
	"testing"

	"songloft/internal/player"
)

// TestReorderMembers 成员顺序：全排列生效、非法输入被拒、不改主控。
//
// 顺序只影响展示与"主控候选"提示。三个边界必须成立：
//  1. 合法全排列按请求生效，并落库（重启后顺序不变）；
//  2. 少一个 / 多一个 / 重复 / 含外人，一律 400，不能悄悄"弄丢"成员；
//  3. 主控不跟着变——拖错了不会突然换人出声。
func TestReorderMembers(t *testing.T) {
	m, _, _ := newTestManager(t, "a", "b", "c")
	ctx := context.Background()
	gid := m.GroupIDOf("a")
	if _, err := m.AddMember(ctx, gid, "b"); err != nil {
		t.Fatalf("add b: %v", err)
	}
	if _, err := m.AddMember(ctx, gid, "c"); err != nil {
		t.Fatalf("add c: %v", err)
	}

	view, err := m.ReorderMembers(ctx, gid, []string{"c", "a", "b"})
	if err != nil {
		t.Fatalf("reorder: %v", err)
	}
	if got := view.MemberIDs(); len(got) != 3 || got[0] != "c" || got[1] != "a" || got[2] != "b" {
		t.Fatalf("顺序未生效：%v", got)
	}
	if view.MasterID != "a" {
		t.Fatalf("重排不应改主控，实际 master=%q", view.MasterID)
	}
	for i, mem := range view.Members {
		if mem.Position != i {
			t.Fatalf("成员 %s position=%d，期望 %d", mem.PlayerID, mem.Position, i)
		}
	}

	bad := [][]string{
		{"c", "a"},           // 少一个
		{"c", "a", "b", "d"}, // 多一个
		{"c", "c", "b"},      // 重复
		{"c", "a", "x"},      // 含组外设备
	}
	for _, order := range bad {
		if _, err := m.ReorderMembers(ctx, gid, order); !errors.Is(err, player.ErrBadMemberOrder) {
			t.Fatalf("非法顺序 %v 应返回 ErrBadMemberOrder，实际 %v", order, err)
		}
	}
	// 失败不能改动现状。
	if got := m.GroupIDOf("a"); got != gid {
		t.Fatalf("a 应仍在组 %s，实际 %s", gid, got)
	}
}
