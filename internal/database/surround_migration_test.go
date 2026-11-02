package database_test

import (
	"context"
	"github.com/pressly/goose/v3"
	"os"
	"songloft/internal/database"
	"songloft/internal/database/testutil"
	"songloft/internal/models"
	"strings"
	"testing"
)

func TestSurroundMigrationPreservesCalibrationAndScanSettings(t *testing.T) {
	ctx := context.Background()
	db := testutil.OpenMemoryDB(t)
	provider, err := goose.NewProvider(goose.DialectSQLite3, db.DB(), os.DirFS("migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 48); err != nil {
		t.Fatal(err)
	}
	repo := db.PlaybackRepository()
	original := database.GroupMemberRow{GroupID: "group", PlayerID: "speaker", Position: 3, DelayMs: 128, VolumeOffset: -4, ChannelMode: "left"}
	if err := repo.UpsertGroupMember(ctx, original); err != nil {
		t.Fatal(err)
	}
	if err := db.ConfigRepository().Set(ctx, &models.Config{Key: "scan_config", Value: `{"supported_formats":["FLAC","DTS"],"auto_scan":false,"custom":123}`}); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := repo.ListGroupMembers(ctx, "group")
	if err != nil || len(rows) != 1 || rows[0] != original {
		t.Fatalf("migration lost member config: %+v %v", rows, err)
	}
	cfg, err := db.ConfigRepository().Get(ctx, "scan_config")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Value != `{"supported_formats":["FLAC","DTS","dtshd"],"auto_scan":false,"custom":123}` {
		t.Fatal(cfg.Value)
	}
	for _, mode := range strings.Split("front_left,front_right,center,lfe,back_left,back_right,back_center,side_left,side_right", ",") {
		original.ChannelMode = mode
		if err := repo.UpsertGroupMember(ctx, original); err != nil {
			t.Fatalf("save %s: %v", mode, err)
		}
	}
	if _, err := provider.DownTo(ctx, 48); err != nil {
		t.Fatal(err)
	}
	rows, err = repo.ListGroupMembers(ctx, "group")
	if err != nil || rows[0].ChannelMode != "stereo" || rows[0].DelayMs != 128 || rows[0].Position != 3 || rows[0].VolumeOffset != -4 {
		t.Fatalf("rollback: %+v %v", rows, err)
	}
}
