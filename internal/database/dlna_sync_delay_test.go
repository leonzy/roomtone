package database

import (
	"context"
	"testing"
)

func TestDLNASyncDelayPersistsAndDiscoveryPreservesIt(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	repo := db.(*SQLiteDB).PlaybackRepository()
	ctx := context.Background()
	row := PlayerRow{ID: "dlna", Kind: "dlna", Name: "小爱", Capabilities: "{}", VolumeMode: "device", TakeoverBehavior: "pause", MaxVolume: 100}
	if err := repo.UpsertPlayer(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdatePlayerConfig(ctx, row.ID, row.Name, "pause", "device", 100, "", 1000); err != nil {
		t.Fatal(err)
	}
	if err := repo.UpsertPlayer(ctx, row); err != nil {
		t.Fatal(err)
	}
	// Existing callers that omit delay must not reset it when renaming.
	if err := repo.UpdatePlayerConfig(ctx, row.ID, "餐厅", "pause", "device", 100, ""); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetPlayer(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DLNASyncDelayMs != 1000 {
		t.Fatalf("delay overwritten: %+v", got)
	}
	for _, bad := range []int{-1, 2001} {
		if err := repo.UpdatePlayerConfig(ctx, row.ID, row.Name, "pause", "device", 100, "", bad); err == nil {
			t.Fatalf("schema accepted %d", bad)
		}
	}
}
