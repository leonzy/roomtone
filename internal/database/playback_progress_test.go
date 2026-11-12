package database_test

import (
	"context"
	"songloft/internal/database"
	"songloft/internal/database/testutil"
	"testing"
)

func TestPlaybackProgressSurvivesRepositoryRecreation(t *testing.T) {
	db := testutil.OpenMemoryDB(t)
	ctx := context.Background()
	original := database.PlaybackStateRow{OwnerKind: "group", OwnerID: "test", SongID: 3, State: "paused", QueueProgress: `{"items":[1,2,3],"started_indices":[0,2]}`}
	if err := db.PlaybackRepository().SaveState(ctx, original); err != nil {
		t.Fatal(err)
	}
	got, err := db.PlaybackRepository().GetState(ctx, "group", "test")
	if err != nil {
		t.Fatal(err)
	}
	if got.QueueProgress != original.QueueProgress {
		t.Fatal(got.QueueProgress)
	}
	original.QueueProgress = ""
	if err := db.PlaybackRepository().SaveState(ctx, original); err != nil {
		t.Fatal(err)
	}
	got, err = db.PlaybackRepository().GetState(ctx, "group", "test")
	if err != nil || got.QueueProgress != "" {
		t.Fatal(got, err)
	}
}
