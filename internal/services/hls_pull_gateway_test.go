package services

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"songloft/internal/httputil"
)

func TestHLSPullGatewayRewritesAndValidatesAllReferences(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.UserAgent() != "test-agent" || r.Referer() != "https://example.com/" {
			http.Error(w, "missing headers", 403)
			return
		}
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/dir/live.m3u8", 302)
		case "/dir/live.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"key.bin\"\n#EXT-X-MAP:URI=\"init.mp4\"\n#EXTINF:4,\nsegment.ts?sig=123\n")
		case "/dir/segment.ts":
			w.Header().Set("Content-Type", "video/mp2t")
			io.WriteString(w, "12345678")
		default:
			io.WriteString(w, "0123456789abcdef")
		}
	}))
	defer upstream.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, err := newHLSPullGateway(ctx, HLSTranscodeOptions{UserAgent: "test-agent", Referer: "https://example.com/"}, func() []string { return []string{"127.0.0.1"} })
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	resp, err := http.Get(g.URL(upstream.URL + "/redirect"))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status=%d: %s", resp.StatusCode, data)
	}
	if strings.Contains(string(data), upstream.URL) {
		t.Fatalf("upstream reference escaped: %s", data)
	}
	base, _ := url.Parse(upstream.URL + "/dir/live.m3u8")
	for _, ref := range []string{"key.bin", "init.mp4", "segment.ts?sig=123"} {
		u, _ := url.Parse(ref)
		if !strings.Contains(string(data), g.URL(base.ResolveReference(u).String())) {
			t.Fatalf("reference not rewritten: %s", data)
		}
	}
	for _, ref := range []string{"file:///etc/passwd", "tcp://127.0.0.1:1234", "http://localhost/secret", "http://169.254.169.254/latest/meta-data/", "http://[::1]/secret"} {
		_, err := g.rewrite([]byte("#EXTM3U\n"+ref+"\n"), base)
		if err == nil {
			t.Fatalf("allowed %s", ref)
		}
		_, err = g.rewrite([]byte("#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\""+ref+"\"\n"), base)
		if err == nil {
			t.Fatalf("allowed key %s", ref)
		}
	}
	for _, tag := range []string{`#EXT-X-KEY:METHOD=AES-128,URI=file:///etc/passwd`, `#EXT-X-DEFINE:NAME="a",VALUE="b"`} {
		if _, err := g.rewrite([]byte("#EXTM3U\n"+tag), base); err == nil {
			t.Fatalf("allowed malformed/unsupported tag %s", tag)
		}
	}
	if calls.Load() != 2 {
		t.Fatal("validation fetched a forbidden reference")
	}
}

func TestHLSPullGatewayPrivateAndRedirectProtection(t *testing.T) {
	var forbidden atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forbidden.Add(1) }))
	defer destination.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(destination.URL, "127.0.0.1", "localhost", 1), 302)
	}))
	defer upstream.Close()
	for _, allow := range []bool{false, true} {
		g, err := newHLSPullGateway(context.Background(), HLSTranscodeOptions{}, func() []string {
			if allow {
				return []string{"127.0.0.1"}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.Get(g.URL(upstream.URL))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		g.Close()
		if resp.StatusCode == 200 {
			t.Fatal("forbidden destination accepted")
		}
		assertHLSStatus(t, g.failure("failed"), 403)
	}
	if forbidden.Load() != 0 {
		t.Fatal("redirect reached forbidden host")
	}
}

func TestHLSPullGatewayGlobalProxy(t *testing.T) {
	var calls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.String() != "http://93.184.216.34/live.ts" {
			t.Errorf("proxy target=%s", r.URL)
		}
		io.WriteString(w, "12345678")
	}))
	defer proxy.Close()
	previous := httputil.GetGlobalProxy()
	if err := httputil.SetGlobalProxy(proxy.URL); err != nil {
		t.Fatal(err)
	}
	defer httputil.SetGlobalProxy(previous)
	g, err := newHLSPullGateway(context.Background(), HLSTranscodeOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	resp, err := http.Get(g.URL("http://93.184.216.34/live.ts"))
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(data) != "12345678" || calls.Load() != 1 {
		t.Fatalf("proxy not used: %q, %d", data, calls.Load())
	}
}

func TestHLSPullGatewayBOMAndMalformedPlaylist(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad.m3u8" {
			io.WriteString(w, "invalid playlist")
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		io.WriteString(w, "\xef\xbb\xbf  #EXTM3U\n#EXTINF:4,\nfile:///etc/passwd\n")
	}))
	defer upstream.Close()
	g, err := newHLSPullGateway(context.Background(), HLSTranscodeOptions{}, func() []string { return []string{"127.0.0.1"} })
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	for _, path := range []string{"/live", "/bad.m3u8"} {
		resp, err := http.Get(g.URL(upstream.URL + path))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Fatalf("accepted unsafe/malformed playlist %s", path)
		}
	}
}
