package services

import (
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"

	"songloft/internal/fileutil"
)

const negativeCUECheckpointKey = "scan_negative_cue_directories_v1"

type negativeCUEDirectory struct {
	Stamp fileutil.DirectoryStamp `json:"stamp"`
	Files map[string]bool         `json:"files"`
}

// Cache only successful negative detections. Open/parse errors must be retried;
// known CUE albums retain the existing explicit-reimport semantics.
type negativeCUECheckpoints struct {
	previous map[string]negativeCUEDirectory
	next     map[string]negativeCUEDirectory
	original string
}

func newNegativeCUECheckpoints(config *ConfigService) *negativeCUECheckpoints {
	c := &negativeCUECheckpoints{previous: make(map[string]negativeCUEDirectory), next: make(map[string]negativeCUEDirectory)}
	if config != nil {
		c.original = config.GetString(negativeCUECheckpointKey, "")
		if json.Unmarshal([]byte(c.original), &c.previous) != nil || c.previous == nil {
			c.previous = make(map[string]negativeCUEDirectory)
		}
	}
	return c
}

func (c *negativeCUECheckpoints) unchanged(ctx context.Context, path string) bool {
	dir := filepath.Dir(path)
	stamp, ok := fileutil.CachedDirectoryStamp(ctx, dir)
	if !ok {
		return false
	}
	previous, ok := c.previous[dir]
	if !ok || previous.Stamp != stamp || !previous.Files[filepath.Base(path)] {
		return false
	}
	c.remember(ctx, path)
	return true
}

func (c *negativeCUECheckpoints) remember(ctx context.Context, path string) {
	dir := filepath.Dir(path)
	stamp, ok := fileutil.CachedDirectoryStamp(ctx, dir)
	if !ok {
		return
	}
	entry := c.next[dir]
	if entry.Files == nil {
		entry = negativeCUEDirectory{Stamp: stamp, Files: make(map[string]bool)}
	}
	entry.Files[filepath.Base(path)] = true
	c.next[dir] = entry
}

func (c *negativeCUECheckpoints) save(ctx context.Context, config *ConfigService) {
	if ctx.Err() != nil || config == nil {
		return
	}
	encoded, err := json.Marshal(c.next)
	if err == nil && string(encoded) != c.original {
		err = config.Set(negativeCUECheckpointKey, string(encoded))
	}
	if err != nil {
		slog.Warn("save negative CUE checkpoints failed", "error", err)
	}
}
