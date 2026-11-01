package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/go-chi/chi/v5"
	"songloft/internal/models"
)

func TestDLNACompleteTranscodeSupportsHeadAndByteRanges(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatal("real ffmpeg required for renderer compatibility regression")
	}
	src := filepath.Join(t.TempDir(), "source.flac")
	if out, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=3", "-ac", "2", src).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, out)
	}
	handler, repo, _ := newSeekTestHandler(t, ffmpeg)
	id := seedSong(t, repo, &models.Song{Type: models.TypeLocal, Title: "DLNA fixture", FilePath: src, Format: "flac", Duration: 3})
	request := func(method, byteRange string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/play?format=mp3", nil)
		route := chi.NewRouteContext()
		route.URLParams.Add("id", strconv.FormatInt(id, 10))
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, route))
		r.Header.Set("Range", byteRange)
		w := httptest.NewRecorder()
		handler.GetSongPlay(w, WithDLNAPlayback(r))
		return w
	}
	full := request(http.MethodGet, "")
	if full.Code != 200 || full.Header().Get("Content-Length") != strconv.Itoa(full.Body.Len()) || full.Header().Get("Accept-Ranges") != "bytes" || full.Body.Len() < 200 {
		t.Fatalf("not a complete seekable representation: %d %v bytes=%d", full.Code, full.Header(), full.Body.Len())
	}
	head := request(http.MethodHead, "")
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != full.Header().Get("Content-Length") {
		t.Fatalf("HEAD mismatch: %d %v", head.Code, head.Header())
	}
	part := request(http.MethodGet, "bytes=100-199")
	if part.Code != 206 || part.Body.String() != full.Body.String()[100:200] {
		t.Fatalf("range mismatch: %d %v", part.Code, part.Header())
	}
	// A decoder failure must not masquerade as a playable MP3 containing FLAC bytes.
	bad := filepath.Join(t.TempDir(), "broken.flac")
	if err := os.WriteFile(bad, []byte("invalid FLAC"), 0600); err != nil {
		t.Fatal(err)
	}
	id = seedSong(t, repo, &models.Song{Type: models.TypeLocal, Title: "broken", FilePath: bad, Format: "flac", Duration: 3})
	failed := request(http.MethodGet, "")
	if failed.Code != http.StatusBadGateway {
		t.Fatalf("unsupported source silently served as audio: %d %s", failed.Code, failed.Body.String())
	}
}
