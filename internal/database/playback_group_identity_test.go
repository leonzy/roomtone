package database_test

import (
	"context"
	"errors"
	"testing"

	"songloft/internal/database"
	"songloft/internal/database/testutil"
)

// TestGroupIdentityRoundTrip 区域名称与图标必须落库，换一个仓储实例（模拟重启）后仍在。
func TestGroupIdentityRoundTrip(t *testing.T) {
	db := testutil.OpenMemoryDB(t)
	ctx := context.Background()
	repo := db.PlaybackRepository()

	if err := repo.InsertGroup(ctx, database.GroupRow{ID: "grp-1", Name: "客厅", MasterPlayerID: "p1"}); err != nil {
		t.Fatalf("insert group: %v", err)
	}
	if err := repo.UpdateGroupIdentity(ctx, "grp-1", "书房", "bookshelf"); err != nil {
		t.Fatalf("update identity: %v", err)
	}

	// 新仓储实例走同一张库，等价于服务重启后重新加载。
	reloaded := database.NewPlaybackRepository(db.DB())
	got, err := reloaded.GetGroup(ctx, "grp-1")
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if got.Name != "书房" || got.Icon != "bookshelf" {
		t.Fatalf("重启后 = name %q icon %q，期望 书房 / bookshelf", got.Name, got.Icon)
	}

	groups, err := reloaded.ListGroups(ctx)
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	if len(groups) != 1 || groups[0].Name != "书房" || groups[0].Icon != "bookshelf" {
		t.Fatalf("列表 = %+v", groups)
	}

	if err := repo.UpdateGroupIdentity(ctx, "grp-missing", "书房", ""); !errors.Is(err, database.ErrNotFound) {
		t.Fatalf("更新不存在的组错误 = %v，期望 ErrNotFound", err)
	}
}
