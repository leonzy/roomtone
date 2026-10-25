package handlers

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"songloft/internal/player"
	"songloft/internal/player/discovery"
)

type fakeDiscovery struct {
	res discovery.Result
	err error
}

func (f fakeDiscovery) Scan(context.Context) (discovery.Result, error) { return f.res, f.err }

func newPlaybackHandlerForTest(t *testing.T) *PlaybackHandler {
	t.Helper()
	mgr := player.NewManager(player.ManagerDeps{})
	return NewPlaybackHandler(mgr, nil)
}

func TestDiscoverPlayersReturnsScanResult(t *testing.T) {
	h := newPlaybackHandlerForTest(t)
	h.SetDiscovery(fakeDiscovery{res: discovery.Result{
		Speakers: []discovery.Speaker{{ID: "raop:aa:bb:cc:dd:ee:ff", Kind: player.KindRAOP, Name: "客厅"}},
		Added:    []string{"raop:aa:bb:cc:dd:ee:ff"},
	}})
	rr := httptest.NewRecorder()
	h.DiscoverPlayers(rr, httptest.NewRequest(http.MethodPost, "/api/v1/players/discover", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200：%s", rr.Code, rr.Body.String())
	}
	var got discovery.Result
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Added) != 1 || got.Added[0] != "raop:aa:bb:cc:dd:ee:ff" {
		t.Errorf("结果透传不对: %+v", got)
	}
}

func TestDiscoverPlayersWithoutDiscovery(t *testing.T) {
	h := newPlaybackHandlerForTest(t)
	rr := httptest.NewRecorder()
	h.DiscoverPlayers(rr, httptest.NewRequest(http.MethodPost, "/api/v1/players/discover", nil))
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("没有发现器时应 503，实际 %d", rr.Code)
	}
}

func TestAddPlayerValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		code int
	}{
		{"坏 JSON", `{`, http.StatusBadRequest},
		{"kind 不支持", `{"address":"10.0.0.2","kind":"local"}`, http.StatusBadRequest},
		{"缺地址", `{"kind":"dlna"}`, http.StatusBadRequest},
		{"RAOP 缺 device_id", `{"address":"10.0.0.2:5000","kind":"raop"}`, http.StatusBadRequest},
		{"DLNA 地址不可达", `{"address":"127.0.0.1:1","kind":"dlna"}`, http.StatusBadRequest},
	}
	h := newPlaybackHandlerForTest(t)
	for _, c := range cases {
		rr := httptest.NewRecorder()
		h.AddPlayer(rr, httptest.NewRequest(http.MethodPost, "/api/v1/players", strings.NewReader(c.body)))
		if rr.Code != c.code {
			t.Errorf("%s：状态码 = %d，期望 %d（%s）", c.name, rr.Code, c.code, rr.Body.String())
		}
	}
}

func TestAddPlayerRejectsPrivateAddressWhenDisallowed(t *testing.T) {
	h := newPlaybackHandlerForTest(t)
	h.SetAddressPolicy(func() bool { return false })
	rr := httptest.NewRecorder()
	body := `{"address":"192.168.1.9:5000","kind":"raop","device_id":"aabbccddeeff"}`
	h.AddPlayer(rr, httptest.NewRequest(http.MethodPost, "/api/v1/players", strings.NewReader(body)))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("关闭私网后应拒绝私网地址，实际 %d：%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "私网") {
		t.Errorf("错误文案应说明是私网限制: %s", rr.Body.String())
	}
}

func TestSplitHostPort(t *testing.T) {
	cases := []struct {
		kind      player.Kind
		address   string
		wantHost  string
		wantPort  string
		wantError bool
	}{
		{player.KindRAOP, "10.0.0.2:7000", "10.0.0.2", "7000", false},
		{player.KindRAOP, "10.0.0.2", "10.0.0.2", "5000", false},
		{player.KindDLNA, "10.0.0.2", "10.0.0.2", "80", false},
		{player.KindDLNA, "http://10.0.0.2:1400/xml/desc.xml", "10.0.0.2", "1400", false},
		{player.KindDLNA, "http://10.0.0.2/desc.xml", "10.0.0.2", "80", false},
		{player.KindDLNA, "fe80::1", "fe80::1", "80", false},
	}
	for _, c := range cases {
		host, port, err := splitHostPort(c.kind, c.address)
		if c.wantError {
			if err == nil {
				t.Errorf("%q 应当报错", c.address)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q 解析失败: %v", c.address, err)
			continue
		}
		if host != c.wantHost || port != c.wantPort {
			t.Errorf("%q → (%q,%q)，期望 (%q,%q)", c.address, host, port, c.wantHost, c.wantPort)
		}
	}
}

func TestReconnectUnknownPlayer(t *testing.T) {
	h := newPlaybackHandlerForTest(t)
	rr := httptest.NewRecorder()
	req := newRouteRequest(http.MethodPost, "/api/v1/players/nope/reconnect", nil, map[string]string{"id": "nope"})
	h.ReconnectPlayer(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("未知设备应 404，实际 %d", rr.Code)
	}
}

func TestIsPrivateIP(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "192.168.0.1", "172.16.0.1", "169.254.1.1", "::1", "0.0.0.0"} {
		if !isPrivateIP(parseIP(t, s)) {
			t.Errorf("%s 应判为私网", s)
		}
	}
	if isPrivateIP(parseIP(t, "8.8.8.8")) {
		t.Error("8.8.8.8 不应判为私网")
	}
}

func parseIP(t *testing.T, s string) net.IP {
	t.Helper()
	ip := net.ParseIP(s)
	if ip == nil {
		t.Fatalf("坏 IP: %s", s)
	}
	return ip
}
