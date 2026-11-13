package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"songloft/internal/database/testutil"
	"songloft/internal/models"
)

type countedCueRepository struct {
	SongRepository
	bulkCalls   int
	singleCalls int
	failBulk    bool
}

func (r *countedCueRepository) ListCueAudioPathsBySource(ctx context.Context) (map[string][]string, error) {
	r.bulkCalls++
	if r.failBulk {
		return nil, errors.New("simulated read failure")
	}
	return r.SongRepository.(interface {
		ListCueAudioPathsBySource(context.Context) (map[string][]string, error)
	}).ListCueAudioPathsBySource(ctx)
}
func (r *countedCueRepository) ListCueAudioPaths(ctx context.Context, source string) ([]string, error) {
	r.singleCalls++
	return r.SongRepository.ListCueAudioPaths(ctx, source)
}

func TestCueCleanupLoadsReferencesOnceAndPreservesOutOfScopeRecords(t *testing.T) {
	db := testutil.OpenMemoryDB(t)
	ctx := context.Background()
	root := t.TempDir()
	outside := t.TempDir()
	audio := filepath.Join(root, "audio.flac")
	if err := os.WriteFile(audio, []byte("audio"), 0600); err != nil {
		t.Fatal(err)
	}
	var songs []*models.Song
	for i := 0; i < 20; i++ {
		dir := filepath.Join(root, string(rune('a'+i)))
		_ = os.MkdirAll(dir, 0755)
		source := filepath.Join(dir, "album.cue")
		if err := os.WriteFile(source, []byte("cue"), 0600); err != nil {
			t.Fatal(err)
		}
		songs = append(songs, &models.Song{Type: models.TypeLocal, Title: "track", FilePath: audio, CueSourcePath: source, CueAudioPath: audio, CueTrackIndex: 1})
	}
	missing := filepath.Join(root, "gone.cue")
	untouched := filepath.Join(outside, "gone.cue")
	for _, source := range []string{missing, untouched} {
		songs = append(songs, &models.Song{Type: models.TypeLocal, Title: "gone", FilePath: audio, CueSourcePath: source, CueAudioPath: audio, CueTrackIndex: 1})
	}
	if err := db.SongRepository().BatchCreate(ctx, songs); err != nil {
		t.Fatal(err)
	}
	counted := &countedCueRepository{SongRepository: db.SongRepository()}
	service := &SongService{songs: counted}
	service.cleanStaleCueRecords(ctx, []string{root})
	if counted.bulkCalls != 1 || counted.singleCalls != 0 {
		t.Fatal("N+1 lookup", counted.bulkCalls, counted.singleCalls)
	}
	sources, err := db.SongRepository().ListCueSources(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sources[missing] || !sources[untouched] || len(sources) != 21 {
		t.Fatal("wrong cleanup scope", len(sources))
	}
	counted.failBulk = true
	service.cleanStaleCueRecords(ctx, nil)
	sources, err = db.SongRepository().ListCueSources(ctx)
	if err != nil || !sources[untouched] {
		t.Fatal("failed read removed records", err)
	}
}
