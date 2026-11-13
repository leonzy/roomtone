package database

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"songloft/internal/fileutil"
	"songloft/internal/models"
)

// Opt-in production-size verification. The database must be a marked COPY;
// the NAS harness mounts music/artwork read-only and never starts the service.
func TestDirectoryCheckpointRealLibrary(t *testing.T) {
	path := os.Getenv("ROOMTONE_DIRECTORY_BENCH_DB")
	if path == "" {
		t.Skip("requires a marked independent database copy")
	}
	if _, err := os.Stat(path + ".benchmark-copy"); err != nil {
		t.Fatal("refusing unmarked database", err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	mode := "directory"
	if cfg, err := db.ConfigRepository().Get(ctx, "scan_playlist_mode"); err == nil {
		mode = cfg.Value
		var parsed string
		if json.Unmarshal([]byte(cfg.Value), &parsed) == nil {
			mode = parsed
		}
	}
	var music struct {
		AutoCreateExcludeDirs []string `json:"auto_create_exclude_dirs"`
	}
	if cfg, err := db.ConfigRepository().Get(ctx, "music_path"); err == nil {
		_ = json.Unmarshal([]byte(cfg.Value), &music)
	}
	// Build the same cheap stamps a scan hands to auto-create, without importing songs.
	// ReadDirectoryStamp below is also used by explicit (non-scan) generation.
	covers := os.Getenv("ROOMTONE_DIRECTORY_BENCH_COVERS")
	if covers == "" {
		covers = "/app/data/covers"
	}
	root := os.Getenv("ROOMTONE_DIRECTORY_BENCH_ROOT")
	if root == "" {
		root = "/app/music"
	}
	for pass := 1; pass <= 2; pass++ {
		calls := 0
		repo := db.PlaylistRepository()
		repo.resolveDirectoryCover = func(dir string, ids []int64, songs map[int64]*models.Song, out string) (string, string, bool) {
			calls++
			return pickDirCover(dir, ids, songs, out)
		}
		started := time.Now()
		result, err := repo.AutoCreate(ctx, mode, music.AutoCreateExcludeDirs, covers, root)
		if err != nil {
			t.Fatal(err)
		}
		saved, err := db.ConfigRepository().Get(ctx, autoDirectoryCheckpointKey)
		if err != nil {
			t.Fatal(err)
		}
		var checkpoints autoDirectoryCheckpoints
		if err := json.Unmarshal([]byte(saved.Value), &checkpoints); err != nil {
			t.Fatal(err)
		}
		t.Logf("REAL_DIRECTORY_CHECKPOINT pass=%d playlists=%d elapsed=%s cover_lookups=%d checkpoints=%d", pass, result.Total, time.Since(started), calls, len(checkpoints.Entries))
	}
	// A caller-supplied immutable scan snapshot uses canonical path keys.
	if stamp, err := fileutil.ReadDirectoryStamp(ctx, root); err == nil {
		provided := fileutil.WithDirectoryStamps(ctx, map[string]fileutil.DirectoryStamp{filepath.Clean(root): stamp})
		if _, ok := fileutil.CachedDirectoryStamp(provided, root); !ok {
			t.Fatal("scan snapshot not reusable")
		}
	}
}
