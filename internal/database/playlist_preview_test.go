package database

import (
	"context"
	"fmt"
	"testing"

	"songloft/internal/models"
)

// A shadow view throws if metadata for an unselected playlist is evaluated.
// This fails the old global membership GROUP BY, without timing assertions.
func TestPlaylistPreviewCountsOnlySelectedPage(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	raw := db.(*SQLiteDB).db
	raw.SetMaxOpenConns(1)
	ctx := context.Background()
	var playlists []*models.Playlist
	var songs []*models.Song
	for i := 0; i < 3; i++ {
		p := &models.Playlist{Name: fmt.Sprintf("preview-%d", i), Type: models.PlaylistTypeNormal}
		if err := db.PlaylistRepository().Create(ctx, p); err != nil {
			t.Fatal(err)
		}
		s := &models.Song{Title: p.Name, Type: models.TypeRemote, FilePath: fmt.Sprintf("/preview/%d", i)}
		if err := db.SongRepository().Create(ctx, s); err != nil {
			t.Fatal(err)
		}
		if err := db.PlaylistSongRepository().AddSong(ctx, p.ID, s.ID, 0); err != nil {
			t.Fatal(err)
		}
		playlists = append(playlists, p)
		songs = append(songs, s)
	}
	if _, err := raw.Exec(fmt.Sprintf(`CREATE TEMP VIEW songs AS SELECT id, CASE WHEN id=%d THEN type ELSE json('invalid json') END AS type FROM main.songs`, songs[0].ID)); err != nil {
		t.Fatal(err)
	}
	for _, random := range []bool{false, true} {
		rows, err := db.PlaylistRepository().List(ctx, &PlaylistFilter{Limit: 1, Random: random, ExcludeIDs: []int64{1, 2}, AvoidIDs: []int64{playlists[1].ID, playlists[2].ID}})
		if err != nil {
			t.Fatalf("unselected song metadata read (random=%v): %v", random, err)
		}
		if len(rows) != 1 || rows[0].ID != playlists[0].ID || rows[0].SongCount != 1 || rows[0].RemoteCount != 1 {
			t.Fatalf("wrong preview counts: %+v", rows)
		}
	}
	if _, err := db.PlaylistRepository().List(ctx, &PlaylistFilter{Limit: 1, Offset: 1, ExcludeIDs: []int64{1, 2}}); err == nil {
		t.Fatal("poisoned selected metadata should fail; guard was not exercised")
	}
}
