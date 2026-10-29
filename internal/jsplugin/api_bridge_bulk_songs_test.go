package jsplugin

import (
	"context"
	"encoding/json"
	"songloft/internal/models"
	"strings"
	"testing"
)

func TestMetadataSongListCanOmitFingerprintWithoutLosingTags(t *testing.T) {
	h := newTestBridgeHandler(t, []string{PermSongsRead}, "metadata")
	song := &models.Song{Type: models.TypeLocal, Title: "title", Artist: "artist", Album: "album", Genre: "genre", Language: "language", Style: "style", Track: "2/9", FilePath: "music/album/song.flac", Fingerprint: strings.Repeat("A", 8192), FingerprintDuration: 120}
	if err := h.db.SongRepository().Create(context.Background(), song); err != nil {
		t.Fatal(err)
	}
	full, err := h.handleSongs("songs.list", `{"limit":100000}`)
	if err != nil {
		t.Fatal(err)
	}
	light, err := h.handleSongs("songs.list", `{"limit":100000,"omitFingerprint":true}`)
	if err != nil {
		t.Fatal(err)
	}
	var songs []models.Song
	if err := json.Unmarshal([]byte(light), &songs); err != nil {
		t.Fatal(err)
	}
	if len(songs) != 1 || songs[0].Fingerprint != "" || songs[0].FingerprintDuration != 0 {
		t.Fatalf("metadata projection: %+v", songs)
	}
	got := songs[0]
	if got.Artist != song.Artist || got.Album != song.Album || got.Track != song.Track || got.Genre != song.Genre || got.FilePath != song.FilePath || got.Language != song.Language || got.Style != song.Style {
		t.Fatal("lost directory/tag metadata")
	}
	if len(full)-len(light) < 8192 {
		t.Fatal("large fingerprint was not excluded")
	}
	detail, err := h.handleSongs("songs.getById", `{"id":`+jsonNumber(song.ID)+`}`)
	if err != nil {
		t.Fatal(err)
	}
	var original models.Song
	json.Unmarshal([]byte(detail), &original)
	if original.Fingerprint != song.Fingerprint {
		t.Fatal("detail lost fingerprint required for AcoustID")
	}
	if !strings.Contains(full, song.Fingerprint) {
		t.Fatal("default list contract changed")
	}
}
func jsonNumber(n int64) string { b, _ := json.Marshal(n); return string(b) }
