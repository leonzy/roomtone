package raat

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"songloft/internal/player/sink"
)

// testPlan 测试用 StreamPlan。
var testPlan = sink.StreamPlan{
	Format: sink.Format{Codec: sink.CodecL16, SampleRate: 44100, Channels: 2, Bits: 16},
}

// TestLoadScriptUnsigned B10①：设备 code_signing_keys 为空（本机补丁版
// RAATServer 形态）→ 直接发 load_script{script, module, name}（不带
// signature/keypair 字段）。
func TestLoadScriptUnsigned(t *testing.T) {
	d := newFakeRAATDevice(t)
	s := openSinkAt(t, d, nil)
	waitReqs(t, d, 4)

	if err := s.LoadScript(context.Background(), "-- dsp", "", "", "mydsp"); err != nil {
		t.Fatalf("LoadScript: %v", err)
	}
	reqs := waitReqs(t, d, 5)
	if reqs[4].name != "load_script" {
		t.Fatalf("第 5 个请求 = %q，期望 load_script", reqs[4].name)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(reqs[4].body, &body); err != nil {
		t.Fatal(err)
	}
	if string(body["script"]) != `"-- dsp"` {
		t.Fatalf("script = %s", body["script"])
	}
	if _, has := body["signature"]; has {
		t.Fatal("无签名路径不应带 signature 字段")
	}
	if _, has := body["keypair"]; has {
		t.Fatal("无签名路径不应带 keypair 字段")
	}
}

// TestLoadScriptSigned B10②：设备 code_signing_keys 非空 → 先向 mock
// raat-sign POST /sign 取签名，再发 load_script{script, signature, keypair,
// module, name}；签名缓存生效（第二次同内容不再请求 mock）。
func TestLoadScriptSigned(t *testing.T) {
	// mock raat-sign
	var signCount int
	var mu sync.Mutex
	signSrv := newMockSignServer(t, &mu, &signCount)

	d := newFakeRAATDevice(t)
	// 设备声明 code_signing_keys
	d.mu.Lock()
	d.infoResp = `{"status":"Success","output":{"supported_formats":"44100/16/2"},"codecs":[],"volume":{"is_supported":true},"transport":{"is_supported":true},"info":{"code_signing_keys":"testkey,otherkey,mockmissing"}}`
	d.mu.Unlock()

	// 缓存文件（独立临时目录）
	cachePath := filepath.Join(t.TempDir(), "raat-sign-cache.json")

	s := New("dev", Options{
		Address: d.addr(), SampleRate: 44100, Channels: 2,
		BufferSize: 2 * time.Second, ReadyThreshold: time.Second,
	})
	// 注入 SignProxy（mock URL + 缓存路径）
	s.signProxy = &SignProxy{
		BaseURL:   signSignURL(t, signSrv),
		CachePath: cachePath,
		HTTP:      signSrv.Client(),
	}
	if err := s.Open(context.Background(), testPlan); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	waitReqs(t, d, 4)

	// 第一次：走签名（mock 被请求 1 次）
	if err := s.LoadScript(context.Background(), "-- dsp v1", "testkey", "m", "n"); err != nil {
		t.Fatalf("LoadScript#1: %v", err)
	}
	mu.Lock()
	if signCount != 1 {
		t.Fatalf("mock sign 请求 %d 次，期望 1", signCount)
	}
	mu.Unlock()
	reqs := waitReqs(t, d, 5)
	if reqs[4].name != "load_script" {
		t.Fatalf("第 5 个请求 = %q", reqs[4].name)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(reqs[4].body, &body); err != nil {
		t.Fatal(err)
	}
	if string(body["signature"]) == "" || string(body["signature"]) == `""` {
		t.Fatal("签名路径应带 signature 字段")
	}
	if string(body["keypair"]) != `"testkey"` {
		t.Fatalf("keypair = %s", body["keypair"])
	}

	// 第二次同内容：缓存生效（mock 不再被请求）
	if err := s.LoadScript(context.Background(), "-- dsp v1", "testkey", "m", "n"); err != nil {
		t.Fatalf("LoadScript#2: %v", err)
	}
	waitReqs(t, d, 6)
	mu.Lock()
	if signCount != 1 {
		t.Fatalf("缓存应生效：mock sign 请求 %d 次，期望仍为 1", signCount)
	}
	mu.Unlock()

	// mock 返回 notFound → 明确错误（keypair 在设备 keys 里但不在 mock
	// 服务端列表——mockmissing）
	if err := s.LoadScript(context.Background(), "-- dsp v2", "mockmissing", "m", "n"); err == nil {
		t.Fatal("notFound 应报错")
	}
}

// TestAppChannel B10③：设备主动发 type=2 {"channel":...,"message":...} →
// OnAppMessage；SendAppMessage → 设备收到
// app_channel_send_message{channel, message}；ForceExit → 设备收到 force_exit。
func TestAppChannel(t *testing.T) {
	d := newFakeRAATDevice(t)
	s := openSinkAt(t, d, nil)
	waitReqs(t, d, 4)

	// OnAppMessage
	got := make(chan [2]string, 1)
	s.ctrl.OnAppMessage = func(channel string, msg json.RawMessage) {
		got <- [2]string{channel, string(msg)}
	}
	// 设备发 app channel 通知
	d.mu.Lock()
	conn := d.ctrlConns()[0]
	d.mu.Unlock()
	sendServerNotification(conn, `{"channel":"test","message":{"foo":1}}`)
	select {
	case g := <-got:
		if g[0] != "test" || g[1] != `{"foo":1}` {
			t.Fatalf("OnAppMessage = %v", g)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("未收到 app channel 消息")
	}

	// SendAppMessage
	if err := s.SendAppMessage(context.Background(), "test", map[string]any{"bar": 2}); err != nil {
		t.Fatal(err)
	}
	reqs := waitReqs(t, d, 5)
	if reqs[4].name != "app_channel_send_message" {
		t.Fatalf("第 5 个请求 = %q，期望 app_channel_send_message", reqs[4].name)
	}
	var body struct {
		Channel string          `json:"channel"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(reqs[4].body, &body); err != nil {
		t.Fatal(err)
	}
	if body.Channel != "test" || string(body.Message) != `{"bar":2}` {
		t.Fatalf("app_channel_send_message = %+v", body)
	}

	// ForceExit
	if err := s.ForceExit(context.Background()); err != nil {
		t.Fatal(err)
	}
	reqs = waitReqs(t, d, 6)
	if reqs[5].name != "force_exit" {
		t.Fatalf("第 6 个请求 = %q，期望 force_exit", reqs[5].name)
	}
}

// --- 助手 ---

func openSinkAt(t *testing.T, d *fakeRAATDevice, _ any) *Sink {
	t.Helper()
	s := New("dev", Options{
		Address: d.addr(), SampleRate: 44100, Channels: 2,
		BufferSize: 2 * time.Second, ReadyThreshold: time.Second,
	})
	if err := s.Open(context.Background(), testPlan); err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// mockSignServer 最小 raat-sign mock。
type mockSignServer struct {
	srv *http.Server
	url string
}

func newMockSignServer(t *testing.T, mu *sync.Mutex, count *int) *mockSignServer {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/sign", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*count++
		mu.Unlock()
		var req struct {
			ContentHashes []string `json:"contentHashes"`
			KeypairNames  []string `json:"keypairNames"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(400)
			return
		}
		// notFound 语义：keypair 不在服务端列表
		known := map[string]bool{"testkey": true, "otherkey": true}
		for _, k := range req.KeypairNames {
			if !known[k] {
				w.WriteHeader(404)
				return
			}
		}
		// 响应 {contentHashes: {hash: {keypair: signature}}}
		hashes := map[string]map[string]string{}
		for _, h := range req.ContentHashes {
			hashes[h] = map[string]string{}
			for _, k := range req.KeypairNames {
				hashes[h][k] = "sig-" + k + "-" + h[:8]
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"contentHashes": hashes})
	})
	srv := &http.Server{Handler: mux}
	ln := newLocalListener(t)
	go srv.Serve(ln) //nolint:errcheck // 测试
	t.Cleanup(func() { srv.Close() })
	return &mockSignServer{srv: srv, url: "http://" + ln.Addr().String()}
}

func (m *mockSignServer) Client() *http.Client { return &http.Client{Timeout: 5 * time.Second} }

func signSignURL(t *testing.T, m *mockSignServer) string { return m.url }

func newLocalListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}
