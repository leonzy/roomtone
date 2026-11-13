package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"songloft/internal/models"
)

func newCheckpointFixture(t *testing.T) (*SQLiteDB, string, string) {
	t.Helper()
	db, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	root := t.TempDir()
	storage := t.TempDir()
	songPath := filepath.Join(root, "album", "a.mp3")
	if err := os.MkdirAll(filepath.Dir(songPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(songPath, []byte("audio"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := db.SongRepository().BatchCreate(context.Background(), []*models.Song{{Type: models.TypeLocal, Title: "a", FilePath: songPath, Duration: 30}}); err != nil {
		t.Fatal(err)
	}
	return db, root, storage
}

func TestDirectoryCheckpointSkipsCoverLookupAcrossRepositoryRecreation(t *testing.T) {
	db, root, storage := newCheckpointFixture(t)
	ctx := context.Background()
	calls := 0
	resolver := func(dir string, ids []int64, songs map[int64]*models.Song, out string) (string, string, bool) {
		calls++
		return pickDirCover(dir, ids, songs, out)
	}
	run := func() *models.AutoCreatePlaylistsResponse {
		// New instance: the fast path must not depend on process-local memory.
		repo := db.PlaylistRepository()
		repo.resolveDirectoryCover = resolver
		result, err := repo.AutoCreate(ctx, models.PlaylistModeDirectory, nil, storage, root)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first := run()
	if calls != 1 {
		t.Fatal("initial lookup", calls)
	}
	before, _ := db.ConfigRepository().Get(ctx, autoDirectoryCheckpointKey)
	changesBefore := int64(0)
	_ = db.db.QueryRow("SELECT total_changes()").Scan(&changesBefore)
	_ = run()
	if calls != 1 {
		t.Fatal("unchanged directory repeated cover lookup", calls)
	}
	after, _ := db.ConfigRepository().Get(ctx, autoDirectoryCheckpointKey)
	if before.Value != after.Value {
		t.Fatal("unchanged checkpoint was rewritten")
	}
	changesAfter := int64(0)
	_ = db.db.QueryRow("SELECT total_changes()").Scan(&changesAfter)
	if changesBefore != changesAfter {
		t.Fatal("warm synchronization wrote database rows", changesBefore, changesAfter)
	}
	if err := os.WriteFile(filepath.Join(root, "album", "cover.jpg"), []byte("image"), 0600); err != nil {
		t.Fatal(err)
	}
	_ = run()
	if calls != 2 {
		t.Fatal("added cover did not invalidate directory", calls)
	}
	playlist, err := db.PlaylistRepository().GetByID(ctx, first.Playlists[0].PlaylistID)
	if err != nil {
		t.Fatal(err)
	}
	if playlist.CoverPath == "" {
		t.Fatal("new cover was not imported")
	}
	_ = run()
	if calls != 2 {
		t.Fatal("warm cover reread", calls)
	}
	if err := os.Remove(playlist.CoverPath); err != nil {
		t.Fatal(err)
	}
	_ = run()
	if calls != 3 {
		t.Fatal("missing cached artwork was not repaired", calls)
	}
	if _, err := os.Stat(playlist.CoverPath); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoryCheckpointScopeIncludesConfiguration(t *testing.T) {
	base := directoryCheckpointScope("directory", "/music", "/covers", []string{"b", "a"})
	if base != directoryCheckpointScope("directory", "/music", "/covers", []string{"a", "b"}) {
		t.Fatal("ordering changed identity")
	}
	for _, scope := range []string{directoryCheckpointScope("bubble_up", "/music", "/covers", []string{"a", "b"}), directoryCheckpointScope("directory", "/other", "/covers", []string{"a", "b"}), directoryCheckpointScope("directory", "/music", "/other", []string{"a", "b"}), directoryCheckpointScope("directory", "/music", "/covers", nil)} {
		if scope == base {
			t.Fatal("config change missed")
		}
	}
}

func TestDirectoryCheckpointNeverCachesFailedCoverResolution(t *testing.T) {
	db, root, storage := newCheckpointFixture(t)
	calls := 0
	repo := db.PlaylistRepository()
	repo.resolveDirectoryCover = func(string, []int64, map[int64]*models.Song, string) (string, string, bool) {
		calls++
		return "", "", false
	}
	for i := 0; i < 2; i++ {
		if _, err := repo.AutoCreate(context.Background(), models.PlaylistModeDirectory, nil, storage, root); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatal("failed read was treated as unchanged", calls)
	}
}
