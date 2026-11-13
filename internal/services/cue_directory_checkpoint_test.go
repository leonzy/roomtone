package services

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"songloft/internal/database/testutil"
	"songloft/internal/fileutil"
)

func TestEmbeddedCUENegativeResultSkipsUnchangedHeadersAcrossRestart(t *testing.T) {
	db := testutil.OpenMemoryDB(t)
	root := t.TempDir()
	path := filepath.Join(root, "a.flac")
	data := append([]byte{'f', 'L', 'a', 'C', 0x80, 0, 0, 34}, make([]byte, 34)...)
	binary.BigEndian.PutUint64(data[18:26], uint64(44100)<<44|uint64(1)<<41|uint64(15)<<36|uint64(44100))
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	logger := slog.Default()
	defer slog.SetDefault(logger)
	var output bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&output, nil)))
	run := func(force bool) string {
		output.Reset()
		scanner := newWalkScanner(root)
		result, err := scanner.ScanFilesWithCue(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		ctx := fileutil.WithDirectoryStamps(context.Background(), result.DirectoryStamps)
		// Recreate the service/config cache, as on a real restart.
		service := NewSongService(db.SongRepository(), nil, nil, scanner, NewConfigService(db.ConfigRepository()), nil)
		service.processEmbeddedCueSheets(ctx, result.AudioFiles, force)
		return output.String()
	}
	if log := run(false); !strings.Contains(log, "headers_checked=1") {
		t.Fatal(log)
	}
	if log := run(false); !strings.Contains(log, "unchanged_files=1 headers_checked=0") {
		t.Fatal(log)
	}
	if log := run(true); !strings.Contains(log, "headers_checked=1") {
		t.Fatal("force import was skipped", log)
	}
	if err := os.WriteFile(path, []byte("not a flac"), 0600); err != nil {
		t.Fatal(err)
	}
	if log := run(false); !strings.Contains(log, "headers_checked=1") {
		t.Fatal("change was skipped", log)
	}
	if log := run(false); !strings.Contains(log, "headers_checked=1") {
		t.Fatal("parse error was cached as no CUE", log)
	}
}
