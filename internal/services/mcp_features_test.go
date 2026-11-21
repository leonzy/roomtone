package services

import (
	"context"
	"testing"
	"time"

	"songloft/internal/database"
	"songloft/internal/database/testutil"
	"songloft/internal/models"
)

func newMCPTestSong(t *testing.T, db database.DB, songType, title, artist, genre string) *models.Song {
	t.Helper()
	song := &models.Song{Type: songType, Title: title, Artist: artist, Genre: genre,
		Duration: 100, FilePath: "music/" + title + ".flac", URL: "https://example.com/" + title}
	if err := db.SongRepository().Create(context.Background(), song); err != nil {
		t.Fatal(err)
	}
	return song
}

func TestListeningHistoryAndRecommendation(t *testing.T) {
	db := testutil.OpenMemoryDB(t)
	ctx := context.Background()
	first := newMCPTestSong(t, db, models.TypeLocal, "first", "周杰伦", "Pop")
	newMCPTestSong(t, db, models.TypeLocal, "second", "孙燕姿", "Pop")
	newMCPTestSong(t, db, models.TypeRadio, "third", "Radio", "")

	history := NewListeningHistoryService(db)
	startedAt := time.Now().Add(-100 * time.Second)
	sessionID, err := history.Start(ctx, ListeningStart{SongID: first.ID, ZoneID: "living", Source: "mcp", StartedAt: startedAt})
	if err != nil {
		t.Fatal(err)
	}
	duplicateID, err := history.Start(ctx, ListeningStart{SongID: first.ID, ZoneID: "living", Source: "server", StartedAt: startedAt})
	if err != nil || duplicateID != sessionID {
		t.Fatalf("duplicate start must reuse session: %q != %q, %v", duplicateID, sessionID, err)
	}
	if err := history.Finish(ctx, sessionID, 100, 100, startedAt.Add(100*time.Second)); err != nil {
		t.Fatal(err)
	}
	page, err := history.List(ctx, database.ListeningEventFilter{ZoneID: "living"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Song.ID != first.ID || page.Items[0].CompletionRatio != 1 {
		t.Fatalf("unexpected history: %+v", page.Items)
	}
	stats, err := history.Stats(ctx, database.ListeningStatFilter{GroupBy: "artist", MinCompletionRatio: 0.8})
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 1 || stats[0].Value != "周杰伦" || stats[0].CompletedCount != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	recommendations, err := NewRecommendationService(db).Recommend(ctx, RecommendationRequest{SeedSongIDs: []int64{first.ID}, Strategy: "balanced", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(recommendations) != 2 {
		t.Fatalf("expected two recommendations, got %d", len(recommendations))
	}
	for _, item := range recommendations {
		if item.Song.Type == models.TypeRadio || item.Song.IsVideo {
			t.Fatalf("radio/video must not be recommended: %+v", item.Song)
		}
	}
}

func TestMetadataUpdateSynchronizesArtists(t *testing.T) {
	db := testutil.OpenMemoryDB(t)
	ctx := context.Background()
	song := newMCPTestSong(t, db, models.TypeLocal, "夜曲", "未知歌手", "Pop")
	if err := db.SongArtistRepository().SetSongArtists(ctx, song.ID, []models.ArtistInput{
		{Name: "未知歌手", Role: models.ArtistRoleArtist}, {Name: "专辑歌手", Role: models.ArtistRoleAlbumArtist},
	}); err != nil {
		t.Fatal(err)
	}
	artist := "周杰伦"
	title := "夜曲（修正版）"
	result, err := NewMetadataUpdateService(db).Update(ctx, MetadataUpdateRequest{
		SongID: song.ID, Patch: MetadataPatch{Artist: &artist, Title: &title}, WriteToFile: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Song.Artist != artist || result.Song.Title != title || result.Changes["artist"] == nil {
		t.Fatalf("unexpected result: %+v", result)
	}
	relations, err := db.SongArtistRepository().GetBySongID(ctx, song.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(relations) != 2 {
		t.Fatalf("album artist must be preserved: %+v", relations)
	}
	var primary, album bool
	for _, relation := range relations {
		primary = primary || (relation.Role == models.ArtistRoleArtist && relation.Artist.Name == artist)
		album = album || relation.Role == models.ArtistRoleAlbumArtist && relation.Artist.Name == "专辑歌手"
	}
	if !primary || !album {
		t.Fatalf("unexpected relations: %+v", relations)
	}
}
