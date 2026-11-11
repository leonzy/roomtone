package database

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"songloft/internal/models"
)

func TestAutoCreateUnchangedDoesNotWrite(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	raw := db.(*SQLiteDB).db
	ctx := context.Background()
	songs := []*models.Song{
		{Type: models.TypeLocal, Title: "01 Alpha", FilePath: "/music/a/01.wav"},
		{Type: models.TypeLocal, Title: "02 Beta", FilePath: "/music/a/02.wav"},
		{Type: models.TypeLocal, Title: "01 Gamma", FilePath: "/music/b/01.wav"},
	}
	if err := db.SongRepository().BatchCreate(ctx, songs); err != nil {
		t.Fatal(err)
	}
	run := func() *models.AutoCreatePlaylistsResponse {
		t.Helper()
		resp, err := db.PlaylistRepository().AutoCreate(ctx, models.PlaylistModeDirectory, nil, "", "/music")
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	first := run()
	if first.Total != 2 {
		t.Fatalf("playlists = %d", first.Total)
	}
	for _, query := range []string{
		`CREATE TABLE audit (playlist_id INTEGER)`,
		`CREATE TRIGGER audit_update AFTER UPDATE ON playlists BEGIN INSERT INTO audit VALUES (new.id); END`,
		`CREATE TRIGGER audit_delete AFTER DELETE ON playlist_songs BEGIN INSERT INTO audit VALUES (old.playlist_id); END`,
		`CREATE TRIGGER audit_insert AFTER INSERT ON playlist_songs BEGIN INSERT INTO audit VALUES (new.playlist_id); END`,
	} {
		if _, err := raw.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	second := run()
	for i := range first.Playlists {
		if first.Playlists[i] != second.Playlists[i] {
			t.Fatal("unchanged result differs")
		}
	}
	var writes int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM audit`).Scan(&writes); err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatalf("unchanged scan performed %d writes", writes)
	}
	// Renaming a title can change sort order even if no files were added.
	if _, err := raw.Exec(`UPDATE songs SET title='00 Beta' WHERE id=?`, songs[1].ID); err != nil {
		t.Fatal(err)
	}
	run()
	var changedID int64
	if err := raw.QueryRow(`SELECT id FROM playlists WHERE name='a'`).Scan(&changedID); err != nil {
		t.Fatal(err)
	}
	var otherWrites int
	if err := raw.QueryRow(`SELECT COUNT(*) FROM audit WHERE playlist_id != ?`, changedID).Scan(&otherWrites); err != nil {
		t.Fatal(err)
	}
	if otherWrites != 0 {
		t.Fatalf("untouched playlist had %d writes", otherWrites)
	}
	var firstID int64
	if err := raw.QueryRow(`SELECT song_id FROM playlist_songs WHERE playlist_id=? ORDER BY position LIMIT 1`, changedID).Scan(&firstID); err != nil {
		t.Fatal(err)
	}
	if firstID != songs[1].ID {
		t.Fatalf("title reorder not reflected: got %d", firstID)
	}
	if _, err := raw.Exec(`DELETE FROM audit`); err != nil {
		t.Fatal(err)
	}
	run()
	if err := raw.QueryRow(`SELECT COUNT(*) FROM audit`).Scan(&writes); err != nil {
		t.Fatal(err)
	}
	if writes != 0 {
		t.Fatalf("repeat after change performed %d writes", writes)
	}
	// Removing a directory must still remove its stale auto-created playlist.
	if _, err := raw.Exec(`DELETE FROM songs WHERE id=?`, songs[2].ID); err != nil {
		t.Fatal(err)
	}
	if got := run().Total; got != 1 {
		t.Fatalf("stale playlist retained: %d", got)
	}
}

func TestAutoCreateDoesNotReadLargePayloadColumns(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	raw := db.(*SQLiteDB).db
	ctx := context.Background()
	if err := db.SongRepository().Create(ctx, &models.Song{Type: models.TypeLocal, Title: "One", FilePath: "/music/a/one.wav"}); err != nil {
		t.Fatal(err)
	}
	// A real SQLite view rejects access to the columns that caused the expensive
	// full-record sort. The lightweight path must never evaluate these columns.
	columns, err := raw.Query(`PRAGMA table_info(songs)`)
	if err != nil {
		t.Fatal(err)
	}
	var selectCols []string
	for columns.Next() {
		var cid, notnull, pk int
		var name, kind string
		var def any
		if err := columns.Scan(&cid, &name, &kind, &notnull, &def, &pk); err != nil {
			t.Fatal(err)
		}
		if name == "fingerprint" || name == "lyric" || name == "source_data" {
			selectCols = append(selectCols, fmt.Sprintf(`json_extract('invalid-json','$') AS "%s"`, name))
		} else {
			selectCols = append(selectCols, fmt.Sprintf(`"%s"`, name))
		}
	}
	if err := columns.Err(); err != nil {
		t.Fatal(err)
	}
	columns.Close()
	if _, err := raw.Exec(`CREATE TEMP VIEW songs AS SELECT ` + strings.Join(selectCols, ",") + ` FROM main.songs`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SongRepository().List(ctx, &SongFilter{Type: models.TypeLocal}); err == nil {
		t.Fatal("poisoned payload guard did not reject full-record read")
	}
	resp, err := db.PlaylistRepository().AutoCreate(ctx, models.PlaylistModeDirectory, nil, "", "/music")
	if err != nil {
		t.Fatalf("auto-playlist read an unused payload: %v", err)
	}
	if resp.Total != 1 {
		t.Fatalf("playlists = %d", resp.Total)
	}
}
