package database_test

import (
	"context"
	"errors"
	"testing"

	"songloft/internal/database"
	"songloft/internal/database/testutil"
)

// TestAlarmRepositoryCRUD 覆盖闹钟仓储的增删改查与 fired 去重字段。
func TestAlarmRepositoryCRUD(t *testing.T) {
	db := testutil.OpenMemoryDB(t)
	repo := db.AlarmRepository()
	ctx := context.Background()

	row := &database.AlarmRow{
		ID: "a1", Name: "起床", TargetID: "group-1", TimeOfDay: "07:30",
		DaysMask: 127, PlaylistID: 3, Volume: 40, FadeSeconds: 30, Enabled: true,
	}
	if err := repo.Create(ctx, row); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := repo.Get(ctx, "a1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.TimeOfDay != "07:30" || got.Volume != 40 || !got.Enabled || got.FadeSeconds != 30 {
		t.Fatalf("字段不一致: %+v", got)
	}

	got.Volume = 60
	got.Enabled = false
	if err := repo.Update(ctx, got); err != nil {
		t.Fatalf("update: %v", err)
	}
	again, _ := repo.Get(ctx, "a1")
	if again.Volume != 60 || again.Enabled {
		t.Fatalf("更新未生效: %+v", again)
	}

	if err := repo.MarkFired(ctx, "a1", "2026-10-26T07:30"); err != nil {
		t.Fatalf("mark fired: %v", err)
	}
	fired, _ := repo.Get(ctx, "a1")
	if fired.LastFiredAt != "2026-10-26T07:30" {
		t.Fatalf("last_fired_at 未写入: %q", fired.LastFiredAt)
	}

	list, err := repo.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}

	if err := repo.Delete(ctx, "a1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := repo.Get(ctx, "a1"); !errors.Is(err, database.ErrNotFound) {
		t.Fatalf("删除后应 ErrNotFound，实际 %v", err)
	}
	if err := repo.Delete(ctx, "a1"); !errors.Is(err, database.ErrNotFound) {
		t.Fatalf("重复删除应 ErrNotFound，实际 %v", err)
	}
}
