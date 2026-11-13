package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"songloft/internal/fileutil"
	"songloft/internal/models"
)

const autoDirectoryCheckpointKey = "auto_playlist_directory_checkpoints_v1"

type autoDirectoryCheckpoint struct {
	Signature  string `json:"signature"`
	PlaylistID int64  `json:"playlist_id"`
	CoverPath  string `json:"cover_path"`
	CoverURL   string `json:"cover_url"`
}

type autoDirectoryCheckpoints struct {
	Scope   string                             `json:"scope"`
	Entries map[string]autoDirectoryCheckpoint `json:"entries"`
}

func directoryCheckpointScope(mode, root, storage string, exclude []string) string {
	excluded := append([]string(nil), exclude...)
	sort.Strings(excluded)
	return checkpointHash([]any{mode, filepath.Clean(root), filepath.Clean(storage), excluded})
}

func checkpointHash(value any) string {
	h := sha256.New()
	_ = json.NewEncoder(h).Encode(value)
	return hex.EncodeToString(h.Sum(nil))
}

func directoryPlaylistSignature(dir, name, desc string, stamp fileutil.DirectoryStamp, ids []int64, songs map[int64]*models.Song) string {
	fallbackPath, fallbackURL := pickSongCover(ids, songs)
	return checkpointHash([]any{filepath.Clean(dir), name, desc, stamp, ids, fallbackPath, fallbackURL})
}

type checkedDirectoryStamp struct {
	stamp fileutil.DirectoryStamp
	valid bool
}

func readPlaylistDirectoryStamp(ctx context.Context, dir string, checked map[string]checkedDirectoryStamp) (fileutil.DirectoryStamp, bool) {
	dir = filepath.Clean(dir)
	if value, ok := checked[dir]; ok {
		return value.stamp, value.valid
	}
	stamp, ok := fileutil.CachedDirectoryStamp(ctx, dir)
	if !ok {
		var err error
		stamp, err = fileutil.ReadDirectoryStamp(ctx, dir)
		ok = err == nil
	}
	checked[dir] = checkedDirectoryStamp{stamp: stamp, valid: ok}
	return stamp, ok
}

func coverCachePresent(path, storage string, checked map[string]bool) bool {
	if path == "" || storage == "" {
		return true
	}
	rel, err := filepath.Rel(storage, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return true
	}
	if exists, ok := checked[path]; ok {
		return exists
	}
	info, err := os.Stat(path)
	exists := err == nil && info.Mode().IsRegular() && info.Size() > 0
	checked[path] = exists
	return exists
}
