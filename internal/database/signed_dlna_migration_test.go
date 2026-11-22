package database_test

import (
	"context"
	"github.com/pressly/goose/v3"
	"os"
	"songloft/internal/database"
	"songloft/internal/database/testutil"
	"testing"
)

func TestSignedDLNAMigrationPreservesExistingPlayerAndMember(t *testing.T) {
	ctx := context.Background()
	db := testutil.OpenMemoryDB(t)
	provider, err := goose.NewProvider(goose.DialectSQLite3, db.DB(), os.DirFS("migrations"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.DownTo(ctx, 50); err != nil {
		t.Fatal(err)
	}
	repo := db.PlaybackRepository()
	row := database.PlayerRow{ID: "d", Kind: "dlna", Name: "DLNA", Capabilities: "{}", Volume: 33, EQProfile: "{\"enabled\":false}", VolumeMode: "device", MaxVolume: 70, TakeoverBehavior: "pause", DLNASyncDelayMs: 1000}
	if err = repo.UpsertPlayer(ctx, row); err != nil {
		t.Fatal(err)
	}
	if err = repo.UpdatePlayerConfig(ctx, "d", "DLNA", "pause", "device", 70, "", 1000); err != nil {
		t.Fatal(err)
	}
	before, err := repo.GetPlayer(ctx, "d")
	if err != nil {
		t.Fatal(err)
	}
	member := database.GroupMemberRow{GroupID: "g", PlayerID: "d", Position: 1, DelayMs: 200, VolumeOffset: -3, ChannelMode: "right"}
	if err = repo.UpsertGroupMember(ctx, member); err != nil {
		t.Fatal(err)
	}
	if _, err = provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := repo.GetPlayer(ctx, "d")
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("player settings changed: %+v %+v", before, after)
	}
	if err = repo.UpdatePlayerConfig(ctx, "d", "DLNA", "pause", "device", 70, "", -500); err != nil {
		t.Fatal(err)
	}
	after, err = repo.GetPlayer(ctx, "d")
	if err != nil || after.DLNASyncDelayMs != -500 {
		t.Fatal(after, err)
	}
	members, err := repo.ListGroupMembers(ctx, "g")
	if err != nil || len(members) != 1 || members[0] != member {
		t.Fatal(members, err)
	}
	if _, err = provider.DownTo(ctx, 50); err != nil {
		t.Fatal(err)
	}
	after, err = repo.GetPlayer(ctx, "d")
	if err != nil || after.DLNASyncDelayMs != 0 {
		t.Fatal("rollback", after, err)
	}
}
