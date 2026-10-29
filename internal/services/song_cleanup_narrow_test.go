package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"songloft/internal/database"
	"songloft/internal/models"
)

type cleanupNarrowRepository struct{ SongRepository }

func (r cleanupNarrowRepository) List(context.Context, *database.SongFilter) ([]*models.Song, error) {
	return nil, errors.New("cleanup must not load full song metadata")
}

func TestCleanInvalidSongsUsesNarrowPathsAndRetainsExistingAudio(t *testing.T) {
	repo := newTestSongRepo(t)
	dir := t.TempDir()
	bad := filepath.Join(dir, "unreadable-audio.flac")
	if err := os.WriteFile(bad, []byte("not a FLAC audio stream"), 0600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.flac")
	songs := []*models.Song{
		{Type: models.TypeLocal, Title: "missing one", FilePath: missing},
		{Type: models.TypeLocal, Title: "missing duplicate", FilePath: missing},
		{Type: models.TypeLocal, Title: "existing undecodable", FilePath: bad},
		{Type: models.TypeLocal, Title: "virtual cue", FilePath: missing, CueSourcePath: filepath.Join(dir, "album.cue")},
		{Type: models.TypeRemote, Title: "remote", FilePath: missing},
	}
	for _, song := range songs {
		if err := repo.Create(context.Background(), song); err != nil {
			t.Fatal(err)
		}
	}
	service := NewSongService(cleanupNarrowRepository{repo}, nil, nil, nil, nil, nil)
	result, err := service.CleanInvalidSongs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 2 || result.FileNotFound != 2 {
		t.Fatalf("cleanup result %+v", result)
	}
	for _, song := range songs[2:] {
		if _, err := repo.GetByID(context.Background(), song.ID); err != nil {
			t.Fatalf("preserved song %q: %v", song.Title, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.CleanInvalidSongs(ctx); err == nil {
		t.Fatal("cancelled cleanup succeeded")
	}
}

func TestCleanupPathMissingRechecksUnlistedFilesAndSymlinks(t *testing.T) {
	dir := t.TempDir()
	cache := make(map[string]map[string]struct{})
	path := filepath.Join(dir, "created-later.flac")
	if !cleanupPathMissing(path, cache) {
		t.Fatal("missing path was not detected")
	}
	if err := os.WriteFile(path, []byte("present"), 0600); err != nil {
		t.Fatal(err)
	}
	if cleanupPathMissing(path, cache) {
		t.Fatal("stale directory listing marked a newly created file missing")
	}
	link := filepath.Join(dir, "link.flac")
	if err := os.Symlink(path, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	cache = make(map[string]map[string]struct{})
	if cleanupPathMissing(link, cache) {
		t.Fatal("valid symlink was marked missing")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if !cleanupPathMissing(link, cache) {
		t.Fatal("dangling symlink was treated as an existing file")
	}
}
