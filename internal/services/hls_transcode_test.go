package services

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func hlsFixture(t *testing.T, codec string, seconds int) string {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}
	file := filepath.Join(t.TempDir(), "source.ts")
	args := []string{"-v", "error", "-f", "lavfi", "-i", "testsrc2=size=96x64:rate=10", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", fmt.Sprint(seconds), "-c:v", codec, "-threads", "1", "-g", "20", "-bf", "0"}
	if codec == "libx264" {
		args = append(args, "-preset", "ultrafast", "-pix_fmt", "yuv420p")
	}
	args = append(args, "-c:a", "mp2", "-b:a", "128k", "-f", "mpegts", file)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v: %s", err, out)
	}
	return file
}

func hlsTestService(t *testing.T) *HLSTranscodeService {
	t.Helper()
	cache := &CacheService{}
	cache.SetFFmpegPath("ffmpeg")
	s, err := NewHLSTranscodeService(cache, func() []string { return []string{"127.0.0.1"} })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func assertHLSStatus(t *testing.T, err error, status int) {
	t.Helper()
	if err == nil || HLSTranscodeStatus(err) != status {
		t.Fatalf("error = %v, want status %d", err, status)
	}
}

func TestHLSTranscodeRealFFmpeg(t *testing.T) {
	// 拉流代理由网关管理；外部环境代理不能截获 ffmpeg 到回环网关的请求。
	t.Setenv("http_proxy", "http://127.0.0.1:1")
	t.Setenv("no_proxy", "")
	file := hlsFixture(t, "libx264", 60)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.UserAgent() != "hls-test" || r.Referer() != "https://example.com/player" {
			http.Error(w, "headers missing", 403)
			return
		}
		http.ServeFile(w, r, file)
	}))
	defer upstream.Close()
	s := hlsTestService(t)
	id, err := s.Create(context.Background(), "owner", HLSTranscodeOptions{URL: upstream.URL + "/live.ts", UserAgent: "hls-test", Referer: "https://example.com/player"})
	if err != nil {
		t.Fatal(err)
	}
	if len(seekStreamSem) != 1 {
		t.Fatalf("slots=%d", len(seekStreamSem))
	}
	playlist, err := s.Playlist(id, "owner")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("playlist:\n%s", playlist)
	var name string
	for line := range strings.SplitSeq(string(playlist), "\n") {
		if hlsSegmentName.MatchString(line) {
			name = line
			break
		}
	}
	f, closeFile, err := s.OpenSegment(id, "owner", name)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(f)
	closeFile()
	if err != nil {
		t.Fatal(err)
	}
	segment := filepath.Join(t.TempDir(), "segment.ts")
	if err := os.WriteFile(segment, data, 0600); err != nil {
		t.Fatal(err)
	}
	probe, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "stream=codec_type,codec_name,channels,sample_rate", "-of", "json", segment).Output()
	if err != nil {
		t.Fatal(err)
	}
	var metadata struct {
		Streams []struct {
			CodecType  string `json:"codec_type"`
			CodecName  string `json:"codec_name"`
			Channels   int    `json:"channels"`
			SampleRate string `json:"sample_rate"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(probe, &metadata); err != nil {
		t.Fatal(err)
	}
	video, audio := false, false
	for _, stream := range metadata.Streams {
		if stream.CodecType == "video" {
			video = stream.CodecName == "h264"
		}
		if stream.CodecType == "audio" {
			audio = stream.CodecName == "aac" && stream.Channels == 2 && stream.SampleRate == "48000"
		}
	}
	if !video || !audio {
		t.Fatalf("wrong codecs: %s", probe)
	}
	// 只比较压缩视频包的 SHA-256。CSV 可能在哈希后附加 MPEG-TS side data，
	// 且分片末包与源文件的 side data 不同；JSON 字段避免把这些元数据当成视频数据。
	packetHashes := func(path string) []string {
		t.Helper()
		out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "v:0", "-show_packets", "-show_entries", "packet=data_hash", "-show_data_hash", "sha256", "-of", "json", path).Output()
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Packets []struct {
				DataHash string `json:"data_hash"`
			} `json:"packets"`
		}
		if err := json.Unmarshal(out, &result); err != nil {
			t.Fatal(err)
		}
		hashes := make([]string, 0, len(result.Packets))
		for i, packet := range result.Packets {
			if !strings.HasPrefix(packet.DataHash, "SHA256:") || len(packet.DataHash) != len("SHA256:")+64 {
				t.Fatalf("video packet %d has invalid SHA-256: %q", i, packet.DataHash)
			}
			hashes = append(hashes, packet.DataHash)
		}
		return hashes
	}
	original, output := packetHashes(file), packetHashes(segment)
	if len(output) == 0 || len(original) < len(output) {
		t.Fatal("no video packets")
	}
	for i := range output {
		if original[i] != output[i] {
			t.Fatalf("video packet %d changed: input=%s output=%s", i, original[i], output[i])
		}
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			if _, err := s.Playlist(id, "owner"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if len(seekStreamSem) != 1 {
		t.Fatal("playlist requests started extra process")
	}
	_, err = s.Playlist(id, "other")
	assertHLSStatus(t, err, 404)
	assertHLSStatus(t, s.Delete(id, "other"), 404)
	for _, bad := range []string{"../playlist.m3u8", "segment-000000000.ts.tmp", "playlist.m3u8"} {
		_, _, err := s.OpenSegment(id, "owner", bad)
		assertHLSStatus(t, err, 400)
	}
	// 持续拉取到窗口滚动，验证直播不会无限增长播放列表或重启进程。
	deadline := time.Now().Add(35 * time.Second)
	for {
		playlist, err = s.Playlist(id, "owner")
		if err != nil {
			t.Fatal(err)
		}
		sequence, count := 0, 0
		for line := range strings.SplitSeq(string(playlist), "\n") {
			if strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:") {
				sequence, _ = strconv.Atoi(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"))
			}
			if hlsSegmentName.MatchString(line) {
				count++
				name = line
			}
		}
		if count > 6 {
			t.Fatalf("playlist grew to %d segments", count)
		}
		if sequence > 0 {
			if count != 6 {
				t.Fatalf("rolling window=%d, want 6", count)
			}
			files, _ := filepath.Glob(filepath.Join(s.root, id, "*.ts"))
			if len(files) > 9 {
				t.Fatalf("retained %d segments, want <=9", len(files))
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("playlist did not advance")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// 文件租约保证删除会话后仍可完成已打开的分片传输。
	f, release, err := s.OpenSegment(id, "owner", name)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.root, id)
	if err := s.Delete(id, "owner"); err != nil {
		t.Fatal(err)
	}
	if len(seekStreamSem) != 0 {
		t.Fatal("slot leaked")
	}
	if _, err := io.ReadAll(f); err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("directory not removed: %v", err)
	}
	_, err = s.Playlist(id, "owner")
	assertHLSStatus(t, err, 404)
}

func TestHLSTranscodeValidationAndFailures(t *testing.T) {
	s := hlsTestService(t)
	for _, opts := range []HLSTranscodeOptions{{URL: "file:///etc/passwd"}, {URL: "http://user:password@example.com/live"}, {URL: "http://example.com/live", AudioBitrate: 123}, {URL: "http://example.com/live", Referer: "bad\r\nheader"}} {
		_, err := s.Create(context.Background(), "owner", opts)
		assertHLSStatus(t, err, 400)
	}
	_, err := s.Create(context.Background(), "", HLSTranscodeOptions{URL: "https://example.com/live"})
	assertHLSStatus(t, err, 401)
	for range cap(seekStreamSem) {
		seekStreamSem <- struct{}{}
	}
	_, err = s.Create(context.Background(), "owner", HLSTranscodeOptions{URL: "https://example.com/live"})
	for range cap(seekStreamSem) {
		<-seekStreamSem
	}
	assertHLSStatus(t, err, 503)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "denied", 403) }))
	defer upstream.Close()
	_, err = s.Create(context.Background(), "owner", HLSTranscodeOptions{URL: upstream.URL})
	assertHLSStatus(t, err, 502)
	if len(seekStreamSem) != 0 {
		t.Fatal("failed startup leaked slot")
	}
	denied, err := NewHLSTranscodeService(s.cache, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = denied.Create(context.Background(), "owner", HLSTranscodeOptions{URL: upstream.URL})
	denied.Close()
	assertHLSStatus(t, err, 403)
	s.Close()
	_, err = s.Create(context.Background(), "owner", HLSTranscodeOptions{URL: upstream.URL})
	assertHLSStatus(t, err, 503)
}

func TestHLSTranscodeUnsupportedVideo(t *testing.T) {
	file := hlsFixture(t, "mpeg2video", 2)
	upstream := httptest.NewServer(http.FileServer(http.Dir(filepath.Dir(file))))
	defer upstream.Close()
	s := hlsTestService(t)
	_, err := s.Create(context.Background(), "owner", HLSTranscodeOptions{URL: upstream.URL + "/source.ts"})
	assertHLSStatus(t, err, 422)
	if len(seekStreamSem) != 0 {
		t.Fatal("probe failure leaked slot")
	}
}

func TestHLSTranscodeStartupCancellationAndTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer upstream.Close()
	for _, cancelRequest := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelRequest), func(t *testing.T) {
			s := hlsTestService(t)
			s.startupTimeout = 100 * time.Millisecond
			ctx := context.Background()
			var cancel context.CancelFunc
			if cancelRequest {
				ctx, cancel = context.WithTimeout(ctx, 40*time.Millisecond)
				defer cancel()
			}
			_, err := s.Create(ctx, "owner", HLSTranscodeOptions{URL: upstream.URL})
			if cancelRequest {
				if ctx.Err() == nil || err != ctx.Err() {
					t.Fatalf("cancellation: %v", err)
				}
			} else {
				assertHLSStatus(t, err, 504)
			}
			if len(seekStreamSem) != 0 {
				t.Fatal("startup leaked slot")
			}
			entries, _ := os.ReadDir(s.root)
			if len(entries) != 0 {
				t.Fatal("startup leaked files")
			}
		})
	}
}

func TestHLSTranscodeIdleFailureAndShutdown(t *testing.T) {
	file := hlsFixture(t, "libx264", 60)
	upstream := httptest.NewServer(http.FileServer(http.Dir(filepath.Dir(file))))
	defer upstream.Close()
	for _, mode := range []string{"idle", "storage", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			s := hlsTestService(t)
			s.idleTimeout = 800 * time.Millisecond
			s.pollInterval = 20 * time.Millisecond
			id, err := s.Create(context.Background(), "owner", HLSTranscodeOptions{URL: upstream.URL + "/source.ts"})
			if err != nil {
				t.Fatal(err)
			}
			session, err := s.get(id, "owner")
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "storage":
				f, err := os.Create(filepath.Join(session.dir, "large.tmp"))
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Truncate(s.maxBytes + 1); err != nil {
					t.Fatal(err)
				}
				f.Close()
				deadline := time.Now().Add(2 * time.Second)
				for {
					_, err := s.Playlist(id, "owner")
					if err != nil {
						assertHLSStatus(t, err, 502)
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("storage limit not enforced")
					}
					time.Sleep(10 * time.Millisecond)
				}
				if err := s.Delete(id, "owner"); err != nil {
					t.Fatal(err)
				}
			case "idle":
				// 即使首播等待超过闲置阈值，也必须成功交付，再开始闲置计时。
				select {
				case <-session.done:
				case <-time.After(3 * time.Second):
					t.Fatal("idle session not reaped")
				}
			case "shutdown":
				s.Close()
			}
			if len(seekStreamSem) != 0 {
				t.Fatalf("%s leaked slot", mode)
			}
			if _, err := os.Stat(session.dir); !os.IsNotExist(err) {
				t.Fatalf("%s leaked directory: %v", mode, err)
			}
		})
	}
}

func TestHLSTranscodeCloseDuringStartup(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { once.Do(func() { close(entered) }); <-r.Context().Done() }))
	defer upstream.Close()
	s := hlsTestService(t)
	result := make(chan error, 1)
	go func() {
		_, err := s.Create(context.Background(), "owner", HLSTranscodeOptions{URL: upstream.URL})
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("probe never requested source")
	}
	s.Close()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("closed service created session")
		}
	case <-time.After(time.Second):
		t.Fatal("create did not stop")
	}
	if len(seekStreamSem) != 0 {
		t.Fatal("shutdown leaked slot")
	}
	if _, err := os.Stat(s.root); !os.IsNotExist(err) {
		t.Fatalf("shutdown leaked root: %v", err)
	}
}
