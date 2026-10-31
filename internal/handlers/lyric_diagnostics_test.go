package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/go-chi/chi/v5"
	"net/http/httptest"
	"songloft/internal/models"
	"songloft/internal/services"
	"testing"
)

type failingLyricSource struct{ status int }

func (f failingLyricSource) SearchLyrics(context.Context, string, string, string, float64, string, string) (*models.LyricPayload, error) {
	return nil, &models.LyricSearchError{Status: f.status, Reasons: []string{"QQ音乐：请求超时；网易云：未找到匹配；LRCLIB：HTTP 503"}}
}

func TestGetSongLyricPreservesProviderDiagnostics(t *testing.T) {
	for _, status := range []int{404, 502} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			repo := newTestSongRepo(t)
			id := seedSong(t, repo, &models.Song{Type: models.TypeLocal, Title: "成龙-明明白白我的心", FilePath: "/missing.flac"})
			h := NewSongHandler(services.NewSongService(repo, nil, nil, nil, nil, nil), nil, nil, nil, nil, nil)
			h.SetLyricSearcher(failingLyricSource{status})
			router := chi.NewRouter()
			router.Get("/songs/{id}/lyric", h.GetSongLyric)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/songs/%d/lyric", id), nil))
			if w.Code != status || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status/cache: %d %s", w.Code, w.Body.String())
			}
			var body map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["detail"] != "QQ音乐：请求超时；网易云：未找到匹配；LRCLIB：HTTP 503" {
				t.Fatal(body)
			}
		})
	}
}
