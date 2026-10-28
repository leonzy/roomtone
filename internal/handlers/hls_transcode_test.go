package handlers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"
	"songloft/internal/database/testutil"
	"songloft/internal/middleware"
	"songloft/internal/services"
)

func TestHLSTranscodeHTTPPlayback(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	dir := t.TempDir()
	// 真实 AES-128 H.264 + MP2 HLS 源，经主列表→子列表→密钥/TS 分片拉取。
	if err := os.WriteFile(filepath.Join(dir, "key.key"), []byte("0123456789abcdef"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key-info"), []byte("key.key\n"+filepath.Join(dir, "key.key")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"-v", "error", "-f", "lavfi", "-i", "testsrc2=size=96x64:rate=10", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "20", "-c:v", "libx264", "-threads", "1", "-preset", "ultrafast", "-g", "20", "-bf", "0", "-c:a", "mp2", "-b:a", "128k", "-f", "hls", "-hls_time", "2", "-hls_list_size", "0", filepath.Join(dir, "media.m3u8")}
	args = append(args[:len(args)-1], "-hls_key_info_file", filepath.Join(dir, "key-info"), args[len(args)-1])
	if output, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("generate HLS: %v: %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(dir, "master.m3u8"), []byte("#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=500000\nmedia.m3u8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var keyRequests atomic.Int32
	files := http.FileServer(http.Dir(dir))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/key.key" {
			keyRequests.Add(1)
		}
		files.ServeHTTP(w, r)
	}))
	defer upstream.Close()
	db := testutil.OpenMemoryDB(t)
	auth, err := services.NewAuthService(db.ConfigRepository(), db.TokenRepository(), "admin", "admin")
	if err != nil {
		t.Fatal(err)
	}
	defer auth.Close()
	login, err := auth.Login(context.Background(), "admin", "admin", "first")
	if err != nil {
		t.Fatal(err)
	}
	other, err := auth.Login(context.Background(), "admin", "admin", "second")
	if err != nil {
		t.Fatal(err)
	}
	cache := services.NewCacheService(t.TempDir(), services.NewConfigService(db.ConfigRepository()))
	cache.SetFFmpegPath("ffmpeg")
	defer cache.Shutdown()
	service, err := services.NewHLSTranscodeService(cache, func() []string { return []string{"127.0.0.1"} })
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	h := NewHLSTranscodeHandler(service, "/music")
	router := chi.NewRouter()
	router.Use(middleware.AuthMiddleware(auth))
	const prefix = "/api/v1/proxy/transcode/hls/sessions"
	router.Post(prefix, h.Create)
	router.Get(prefix+"/{id}/playlist.m3u8", h.Playlist)
	router.Head(prefix+"/{id}/playlist.m3u8", h.Playlist)
	router.Get(prefix+"/{id}/segments/{name}", h.Segment)
	router.Head(prefix+"/{id}/segments/{name}", h.Segment)
	router.Delete(prefix+"/{id}", h.Delete)
	mux := http.NewServeMux()
	mux.Handle("/music/", http.StripPrefix("/music", router))
	server := httptest.NewServer(mux)
	defer server.Close()
	request := func(method, path, token, body, byteRange string) (int, http.Header, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if byteRange != "" {
			req.Header.Set("Range", byteRange)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, resp.Header, data
	}
	createPath := "/music" + prefix
	if code, _, _ := request("POST", createPath, "", "{}", ""); code != 401 {
		t.Fatalf("unauthenticated=%d", code)
	}
	for _, body := range []string{`{"url":"https://example.com","unknown":1}`, `{} {}`, `null`, `{"url":"file:///etc/passwd"}`} {
		if code, _, data := request("POST", createPath, login.AccessToken, body, ""); code != 400 {
			t.Fatalf("invalid request=%d %s", code, data)
		}
	}
	body, _ := json.Marshal(services.HLSTranscodeOptions{URL: upstream.URL + "/master.m3u8"})
	code, headers, data := request("POST", createPath, login.AccessToken, string(body), "")
	if code != 201 {
		t.Fatalf("create=%d: %s", code, data)
	}
	if keyRequests.Load() == 0 {
		t.Fatal("encrypted HLS key was not fetched")
	}
	var session hlsTranscodeSessionResponse
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(session.PlaylistURL, createPath+"/") || headers.Get("Location") != session.PlaylistURL || session.IdleTimeoutSeconds != 120 {
		t.Fatalf("session=%+v headers=%v", session, headers)
	}
	if code, _, _ := request("GET", session.PlaylistURL, other.AccessToken, "", ""); code != 404 {
		t.Fatalf("owner protection=%d", code)
	}
	if code, _, _ := request("GET", session.PlaylistURL, "", "", ""); code != 401 {
		t.Fatalf("playlist auth=%d", code)
	}
	playlistURL := session.PlaylistURL + "?access_token=" + url.QueryEscape(login.AccessToken)
	code, headers, data = request("GET", playlistURL, "", "", "")
	if code != 200 || headers.Get("Content-Type") != "application/vnd.apple.mpegurl" || headers.Get("Cache-Control") != "no-store" {
		t.Fatalf("playlist=%d %s", code, data)
	}
	var segmentRef string
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.HasPrefix(line, "segments/") {
			segmentRef = line
			break
		}
	}
	if !strings.Contains(segmentRef, "access_token=") {
		t.Fatalf("missing segment auth: %s", data)
	}
	base, _ := url.Parse(playlistURL)
	ref, _ := url.Parse(segmentRef)
	segmentURL := base.ResolveReference(ref).String()
	code, headers, data = request("GET", segmentURL, "", "", "bytes=0-187")
	if code != 206 || len(data) != 188 || data[0] != 0x47 || headers.Get("Content-Type") != "video/mp2t" {
		t.Fatalf("segment=%d, len=%d, headers=%v", code, len(data), headers)
	}
	if code, _, data := request("HEAD", segmentURL, "", "", ""); code != 200 || len(data) != 0 {
		t.Fatalf("HEAD=%d %s", code, data)
	}
	if code, _, _ := request("DELETE", createPath+"/"+session.ID, other.AccessToken, "", ""); code != 404 {
		t.Fatalf("delete owner=%d", code)
	}
	if code, _, _ := request("DELETE", createPath+"/"+session.ID, login.AccessToken, "", ""); code != 204 {
		t.Fatalf("delete=%d", code)
	}
	if code, _, _ := request("GET", playlistURL, "", "", ""); code != 404 {
		t.Fatalf("deleted playlist=%d", code)
	}
}
