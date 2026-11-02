package database

import (
	"context"
	"testing"
)

func TestGroupChannelModeDefaultAndPersistence(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	repo := db.(*SQLiteDB).PlaybackRepository()
	ctx := context.Background()
	for _, m := range []GroupMemberRow{{GroupID: "g", PlayerID: "a"}, {GroupID: "g", PlayerID: "b", ChannelMode: "right"}} {
		if err := repo.UpsertGroupMember(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := repo.ListGroupMembers(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].ChannelMode != "stereo" || rows[1].ChannelMode != "right" {
		t.Fatal(rows)
	}
	if err := repo.UpdateGroupMemberPosition(ctx, "g", "b", 3); err != nil {
		t.Fatal(err)
	}
	rows, err = repo.ListAllGroupMembers(ctx)
	if err != nil || rows[1].ChannelMode != "right" {
		t.Fatalf("restore: %v %v", rows, err)
	}
	if err := repo.UpsertGroupMember(ctx, GroupMemberRow{GroupID: "g", PlayerID: "a", ChannelMode: "surround"}); err == nil {
		t.Fatal("invalid channel accepted")
	}
}
