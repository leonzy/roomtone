package handlers

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"songloft/internal/models"
	"strings"
	"testing"
)

func TestPlaylistRandomPreviewAvoidsPreviousBatch(t *testing.T) {
	env := newPlaylistHandlerEnv(t)
	svc := env.newService()
	h := NewPlaylistHandler(svc, nil)
	for i := 0; i < 12; i++ {
		createTestPlaylist(t, svc, &models.Playlist{Type: models.PlaylistTypeNormal, Name: fmt.Sprintf("sample-%02d", i)})
	}
	createTestPlaylist(t, svc, &models.Playlist{Type: models.PlaylistTypeRadio, Name: "radio sample"})
	createTestPlaylist(t, svc, &models.Playlist{Type: models.PlaylistTypeNormal, Name: "hidden sample", Labels: []string{models.PlaylistLabelHidden}})
	get := func(query string) ([]models.Playlist, int64, int) {
		t.Helper()
		rr := httptest.NewRecorder()
		h.ListPlaylists(rr, httptest.NewRequest("GET", "/api/v1/playlists?type=normal&exclude_ids=1,2&random=true&"+query, nil))
		if rr.Code != 200 {
			t.Fatalf("preview failed %d %s", rr.Code, rr.Body.String())
		}
		var result struct {
			Playlists []models.Playlist `json:"playlists"`
			Total     int64             `json:"total"`
			Limit     int               `json:"limit"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.Playlists, result.Total, result.Limit
	}
	first, total, _ := get("limit=5")
	if len(first) != 5 || total != 12 {
		t.Fatalf("wrong sample: %d/%d", len(first), total)
	}
	seen := map[int64]bool{}
	avoid := []string{}
	for _, p := range first {
		seen[p.ID] = true
		avoid = append(avoid, fmt.Sprint(p.ID))
	}
	second, total, _ := get("limit=5&avoid_ids=" + strings.Join(avoid, ","))
	if len(second) != 5 || total != 12 {
		t.Fatalf("wrong replacement size: %d/%d", len(second), total)
	}
	for _, p := range second {
		if seen[p.ID] {
			t.Fatal("repeated previous batch despite enough alternatives")
		}
	}
	all, total, limit := get("limit=999&avoid_ids=" + strings.Join(avoid, ","))
	if len(all) != 12 || total != 12 || limit != 200 {
		t.Fatalf("small pool or cap: size=%d total=%d limit=%d", len(all), total, limit)
	}
	unique := map[int64]bool{}
	for _, p := range all {
		unique[p.ID] = true
	}
	if len(unique) != 12 {
		t.Fatal("duplicate sampled IDs")
	}
}

func TestPlaylistPreviewRejectsInvalidOptions(t *testing.T) {
	h := NewPlaylistHandler(newPlaylistHandlerEnv(t).newService(), nil)
	for _, query := range []string{"random=maybe", "avoid_ids=0", "exclude_ids=-1", "avoid_ids=" + url.QueryEscape("1);DROP TABLE playlists"), "exclude_ids=" + strings.Repeat("1,", 200) + "1"} {
		rr := httptest.NewRecorder()
		h.ListPlaylists(rr, httptest.NewRequest("GET", "/api/v1/playlists?"+query, nil))
		if rr.Code != 400 {
			t.Fatalf("accepted invalid preview %q: %d", query, rr.Code)
		}
	}
}
