package services

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"songloft/internal/database"
	"songloft/internal/database/testutil"
	"songloft/internal/models"
	"testing"
	"time"
)

func TestScanAndImportDTSWithDefaultConfig(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	db := testutil.OpenMemoryDB(t)
	var scanConfig struct {
		SupportedFormats []string `json:"supported_formats"`
	}
	if err := NewConfigService(db.ConfigRepository()).GetJSON("scan_config", &scanConfig); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	// A real raw DTS stream tests demux, not just the suffix allowlist.
	raw := filepath.Join(root, "raw.DTS")
	if b, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "dca", "-strict", "-2", "-f", "dts", raw).CombinedOutput(); err != nil {
		t.Fatalf("DTS fixture: %v %s", err, b)
	}
	expected := 1
	if sample := os.Getenv("ROOMTONE_DTS_SAMPLE"); sample != "" {
		b, err := os.ReadFile(sample)
		if err != nil {
			t.Fatal(err)
		}
		// The supplied sample is DTS in WAV. Both extensions must retain codec=dts
		// so browser clients ask for a decoded stream rather than raw WAV bytes.
		for _, name := range []string{"sample.dts", "sample.wav"} {
			if err := os.WriteFile(filepath.Join(root, name), b, 0600); err != nil {
				t.Fatal(err)
			}
		}
		expected += 2
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		old := time.Now().Add(-time.Minute)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	scanner := NewScanner(&ScanConfig{MusicPath: root, SupportedFormats: scanConfig.SupportedFormats})
	extractor := NewMetadataExtractor(&MetadataConfig{FFProbePath: ffprobe, CoverStoragePath: t.TempDir()})
	service := NewSongService(db.SongRepository(), db, extractor, scanner, nil, nil)
	service.doScanAndImport(context.Background(), false, nil)
	songs, err := db.SongRepository().List(context.Background(), &database.SongFilter{Type: models.TypeLocal})
	if err != nil {
		t.Fatal(err)
	}
	if len(songs) != expected {
		t.Fatalf("imported %d, want %d; progress=%+v", len(songs), expected, service.GetScanProgress())
	}
	for _, song := range songs {
		if song.Format != "dts" || song.Duration <= 0 {
			t.Fatalf("DTS metadata: %+v", song)
		}
		t.Logf("imported: %s format=%s duration=%.3f", song.Title, song.Format, song.Duration)
	}
}
