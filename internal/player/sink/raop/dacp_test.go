package raop

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"songloft/internal/player/sink"
)

func get(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec // 测试内网地址
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	return resp.StatusCode
}

// TestSinkDACPHeaders Open 启用 OnControl 后，RTSP 请求携带 DACP-ID /
// Active-Remote 头（对齐 AirPlayClientV1.CreateRequest:2884-2893），
// 且反控 HTTP 端点在 RECORD 后就绪。
func TestSinkDACPHeaders(t *testing.T) {
	f := newRecordingRTSP(t)
	s := New("dev", Options{
		Address: f.addr(), SampleRate: 44100, Channels: 2, Latency: 2 * time.Second,
		OnControl: func(string, float64) {},
	})
	if err := s.Open(context.Background(), sink.StreamPlan{
		Format: sink.Format{Codec: sink.CodecL16, SampleRate: 44100, Channels: 2, Bits: 16},
	}); err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	if err := s.UpdateProgress(time.Second, time.Minute); err != nil {
		t.Fatalf("UpdateProgress: %v", err)
	}
	reqs := waitForReqs(t, "携带 DACP 头的请求", f, "text/parameters", 1)
	r := reqs[len(reqs)-1]
	if r.headers["dacp-id"] == "" {
		t.Fatalf("缺少 DACP-ID 头: %v", r.headers)
	}
	if r.headers["active-remote"] == "" {
		t.Fatalf("缺少 Active-Remote 头: %v", r.headers)
	}
	// 反控端点已监听且应答 204。
	if s.dacp == nil || s.dacp.Port() == 0 {
		t.Fatal("DACP 端点未启动")
	}
	if c := get(t, fmt.Sprintf("http://127.0.0.1:%d/ctrl-int/1/pause", s.dacp.Port())); c != 204 {
		t.Fatalf("pause 状态码 = %d，期望 204", c)
	}
	// 头的值与端点身份一致。
	if r.headers["dacp-id"] != s.dacp.ID || r.headers["active-remote"] != s.dacp.ActiveRemote {
		t.Fatalf("DACP 头与端点不一致: %v vs ID=%s AR=%s", r.headers, s.dacp.ID, s.dacp.ActiveRemote)
	}
}
