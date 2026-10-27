package database

import (
	"context"
	"testing"
)

// TestPlayerVolumeModeRoundTrip 音量模式与上限必须落库，且重新注册不能覆盖用户设置。
func TestPlayerVolumeModeRoundTrip(t *testing.T) {
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
		Volume: 80, VolumeMode: "fixed", MaxVolume: 60, TakeoverBehavior: "pause",
	}
	if err := repo.UpsertPlayer(ctx, row); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, err := repo.GetPlayer(ctx, "p1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.VolumeMode != "fixed" || got.MaxVolume != 60 {
		t.Fatalf("落库值 = mode %q / max %d，期望 fixed / 60", got.VolumeMode, got.MaxVolume)
	}

	// 重新注册（发现流程 / 客户端重连）只刷新在线状态，不能把用户设置冲掉。
	row.VolumeMode = "device"
	row.MaxVolume = 100
	if err := repo.UpsertPlayer(ctx, row); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	got, err = repo.GetPlayer(ctx, "p1")
	if err != nil {
		t.Fatalf("get after re-upsert: %v", err)
	}
	if got.VolumeMode != "fixed" || got.MaxVolume != 60 {
		t.Fatalf("重新注册覆盖了用户设置：mode %q / max %d", got.VolumeMode, got.MaxVolume)
	}

	if err := repo.UpdatePlayerConfig(ctx, "p1", "书房", "follow", "dsp", 45, "pw"); err != nil {
		t.Fatalf("update config: %v", err)
	}
	got, err = repo.GetPlayer(ctx, "p1")
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if got.Name != "书房" || got.TakeoverBehavior != "follow" || got.VolumeMode != "dsp" || got.MaxVolume != 45 {
		t.Fatalf("更新后 = %+v", got)
	}
}
