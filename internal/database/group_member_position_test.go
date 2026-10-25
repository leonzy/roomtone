package database

import (
	"context"
	"testing"
)

// TestGroupMemberPositionRoundTrip 成员顺序必须落库并能按顺序读回。
//
// 顺序决定前端列表与"主控候选"提示，只存在内存里的话重启就乱，
// 多端也会各看各的顺序。这里走真实迁移 + 真实仓储。
func TestGroupMemberPositionRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	sqlite, ok := db.(*SQLiteDB)
	if !ok {
		t.Fatalf("期望 *SQLiteDB，实际 %T", db)
	}
	repo := sqlite.PlaybackRepository()
	ctx := context.Background()

	for _, id := range []string{"c", "a", "b"} {
		if err := repo.UpsertGroupMember(ctx, GroupMemberRow{
			GroupID: "g1", PlayerID: id, Position: map[string]int{"c": 0, "a": 1, "b": 2}[id],
		}); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}

	rows, err := repo.ListGroupMembers(ctx, "g1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.PlayerID)
	}
	want := []string{"c", "a", "b"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("顺序 = %v，期望 %v", got, want)
		}
	}

	// 改延迟不能把位置冲掉（UpdateMember 走的也是这个 upsert）。
	if err := repo.UpsertGroupMember(ctx, GroupMemberRow{GroupID: "g1", PlayerID: "a", DelayMs: 120}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := repo.UpdateGroupMemberPosition(ctx, "g1", "b", 0); err != nil {
		t.Fatalf("reposition: %v", err)
	}
	rows, err = repo.ListGroupMembers(ctx, "g1")
	if err != nil {
		t.Fatalf("list2: %v", err)
	}
	if rows[0].PlayerID != "b" {
		t.Fatalf("重排后首项应为 b，实际 %s", rows[0].PlayerID)
	}
	for _, r := range rows {
		if r.PlayerID == "a" && r.Position != 1 {
			t.Fatalf("改延迟不应改动位置，a 的 position=%d", r.Position)
		}
	}
}
