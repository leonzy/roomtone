package handlers

import (
	"bytes"
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
	format := "mp3"
	request := func(method, byteRange string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/play?format="+format, nil)
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
	for _, f := range []string{"l16", "adts"} {
		format = f
		result := request(http.MethodGet, "")
		if result.Code != 200 || result.Body.Len() < 200 || result.Header().Get("Content-Length") != strconv.Itoa(result.Body.Len()) {
			t.Fatalf("%s not a complete representation: %d", f, result.Code)
		}
		if f == "l16" {
			if result.Header().Get("Content-Type") != "audio/L16;rate=44100;channels=2" {
				t.Fatal(result.Header())
			}
			want, err := exec.Command(ffmpeg, "-v", "error", "-i", src, "-ar", "44100", "-ac", "2", "-c:a", "pcm_s16be", "-f", "s16be", "pipe:1").Output()
			if err != nil || !bytes.Equal(result.Body.Bytes(), want) {
				t.Fatal("L16 wire bytes differ from big-endian reference", err)
			}
		} else {
			if result.Header().Get("Content-Type") != "audio/aac" || result.Body.Bytes()[0] != 0xff || result.Body.Bytes()[1]&0xf6 != 0xf0 {
				t.Fatal("ADTS declared but bytes are not AAC ADTS")
			}
			aac := filepath.Join(t.TempDir(), "raw.aac")
			if err := os.WriteFile(aac, result.Body.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			originalID := id
			id = seedSong(t, repo, &models.Song{Type: models.TypeLocal, Title: "ADTS mislabeled as M4A", FilePath: aac, Format: "m4a", Duration: 3})
			format = "m4a"
			remux := request(http.MethodGet, "")
			if remux.Code != 200 || remux.Body.Len() < 12 || string(remux.Body.Bytes()[4:8]) != "ftyp" {
				t.Fatal("raw ADTS was mislabeled as MP4 instead of remuxed")
			}
			id, format = originalID, f
		}
		head := request(http.MethodHead, "")
		part := request(http.MethodGet, "bytes=100-199")
		if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != result.Header().Get("Content-Length") || part.Code != 206 || !bytes.Equal(part.Body.Bytes(), result.Body.Bytes()[100:200]) {
			t.Fatalf("%s HEAD/Range mismatch", f)
		}
	}
	format = "mp3"
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
