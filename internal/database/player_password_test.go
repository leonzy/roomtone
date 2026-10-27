package database

import (
	"context"
	"testing"
)

// TestPlayerPasswordRoundTrip A4：密码列落库——首次注册写入、重新注册
// 不覆盖用户设置、UpdatePlayerConfig 可改。
func TestPlayerPasswordRoundTrip(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	sqlite, ok := db.(*SQLiteDB)
	if !ok {
		t.Fatalf("期望 *SQLiteDB，实际 %T", db)
	}
	repo := sqlite.PlaybackRepository()
	ctx := context.Background()

	row := PlayerRow{
		ID: "p1", Kind: "raop", Name: "客厅", Capabilities: "{}",
		Volume: 80, TakeoverBehavior: "pause", VolumeMode: "device", Password: "s3cret",
	}
	if err := repo.UpsertPlayer(ctx, row); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := repo.GetPlayer(ctx, "p1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Password != "s3cret" {
		t.Fatalf("落库密码 = %q，期望 s3cret", got.Password)
	}

	// 重新注册（发现流程）不能把用户设置的密码冲掉。
	row.Password = ""
	if err := repo.UpsertPlayer(ctx, row); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got, err = repo.GetPlayer(ctx, "p1")
	if err != nil {
		t.Fatalf("get after re-upsert: %v", err)
	}
	if got.Password != "s3cret" {
		t.Fatalf("重新注册覆盖了密码: %q", got.Password)
	}

	// UpdatePlayerConfig 改密码（空串=清空语义由调用方决定，这里验证透传）。
	if err := repo.UpdatePlayerConfig(ctx, "p1", "书房", "follow", "device", 100, "newpw"); err != nil {
		t.Fatalf("update config: %v", err)
	}
	got, err = repo.GetPlayer(ctx, "p1")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.Password != "newpw" || got.Name != "书房" {
		t.Fatalf("更新后 = name %q password %q", got.Name, got.Password)
	}
}
