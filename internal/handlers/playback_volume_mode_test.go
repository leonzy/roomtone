package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"songloft/internal/player"
)

// TestUpdatePlayerRejectsInvalidVolumeMode 非法 volume_mode 必须 400，而不是静默落库。
func TestUpdatePlayerRejectsInvalidVolumeMode(t *testing.T) {
	h := newPlaybackHandlerForTest(t)
	if err := h.mgr.RegisterPlayer(context.Background(), player.Player{ID: "p1", Kind: player.KindRAOP, Name: "客厅"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	rr := httptest.NewRecorder()
	body := []byte(`{"volume_mode":"hardware"}`)
	h.UpdatePlayer(rr, withURLParams(httptest.NewRequest(http.MethodPut, "/api/v1/players/p1", bytes.NewReader(body)), "id", "p1"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400：%s", rr.Code, rr.Body.String())
	}
}

// TestUpdatePlayerRejectsInvalidMaxVolume max_volume 越界必须 400。
func TestUpdatePlayerRejectsInvalidMaxVolume(t *testing.T) {
	h := newPlaybackHandlerForTest(t)
	if err := h.mgr.RegisterPlayer(context.Background(), player.Player{ID: "p1", Kind: player.KindRAOP, Name: "客厅"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	for _, body := range []string{`{"max_volume":-1}`, `{"max_volume":101}`} {
		rr := httptest.NewRecorder()
		h.UpdatePlayer(rr, withURLParams(httptest.NewRequest(http.MethodPut, "/api/v1/players/p1", bytes.NewReader([]byte(body))), "id", "p1"))
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s 状态码 = %d，期望 400：%s", body, rr.Code, rr.Body.String())
		}
	}
}

// TestUpdatePlayerVolumeModeAndMaxVolume 验收：PUT 后 GET /players 返回新值，
// 当前音量随上限钳位，fixed 模式下后续音量请求被忽略。
func TestUpdatePlayerVolumeModeAndMaxVolume(t *testing.T) {
	h := newPlaybackHandlerForTest(t)
	ctx := context.Background()
	if err := h.mgr.RegisterPlayer(ctx, player.Player{ID: "p1", Kind: player.KindRAOP, Name: "客厅"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := h.mgr.SetVolume(ctx, "p1", 80, false); err != nil {
		t.Fatalf("set initial volume: %v", err)
	}

	rr := httptest.NewRecorder()
	body := []byte(`{"volume_mode":"fixed","max_volume":60}`)
	h.UpdatePlayer(rr, withURLParams(httptest.NewRequest(http.MethodPut, "/api/v1/players/p1", bytes.NewReader(body)), "id", "p1"))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT 状态码 = %d：%s", rr.Code, rr.Body.String())
	}
	var updated player.Player
	if err := json.Unmarshal(rr.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode PUT: %v", err)
	}
	if updated.VolumeMode != player.VolumeModeFixed || updated.MaxVolume != 60 || updated.Volume != 60 {
		t.Fatalf("PUT 返回 = mode %q / max %d / volume %d，期望 fixed / 60 / 60",
			updated.VolumeMode, updated.MaxVolume, updated.Volume)
	}

	rr = httptest.NewRecorder()
	h.ListPlayers(rr, httptest.NewRequest(http.MethodGet, "/api/v1/players", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET 状态码 = %d：%s", rr.Code, rr.Body.String())
	}
	var list []player.Player
	if err := json.Unmarshal(rr.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode GET: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("设备数 = %d，期望 1", len(list))
	}
	if list[0].VolumeMode != player.VolumeModeFixed || list[0].MaxVolume != 60 || list[0].Volume != 60 {
		t.Fatalf("GET 返回 = mode %q / max %d / volume %d，期望 fixed / 60 / 60",
			list[0].VolumeMode, list[0].MaxVolume, list[0].Volume)
	}

	// fixed 模式忽略音量设置：请求 80 仍返回 60，且不报错。
	rr = httptest.NewRecorder()
	h.SetVolume(rr, withURLParams(httptest.NewRequest(http.MethodPost, "/api/v1/players/p1/volume", bytes.NewReader([]byte(`{"volume":80}`))), "id", "p1"))
	if rr.Code != http.StatusOK {
		t.Fatalf("fixed 模式音量请求状态码 = %d：%s", rr.Code, rr.Body.String())
	}
	var after player.Player
	if err := json.Unmarshal(rr.Body.Bytes(), &after); err != nil {
		t.Fatalf("decode volume: %v", err)
	}
	if after.Volume != 60 {
		t.Fatalf("fixed 模式音量 = %d，期望保持 60", after.Volume)
	}
}

// TestSetVolumeClampsToMaxVolumeEndpoint device 模式下音量请求被 max_volume 钳位。
func TestSetVolumeClampsToMaxVolumeEndpoint(t *testing.T) {
	h := newPlaybackHandlerForTest(t)
	ctx := context.Background()
	if err := h.mgr.RegisterPlayer(ctx, player.Player{ID: "p1", Kind: player.KindRAOP, Name: "客厅"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	rr := httptest.NewRecorder()
	h.UpdatePlayer(rr, withURLParams(httptest.NewRequest(http.MethodPut, "/api/v1/players/p1", bytes.NewReader([]byte(`{"max_volume":60}`))), "id", "p1"))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT 状态码 = %d：%s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	h.SetVolume(rr, withURLParams(httptest.NewRequest(http.MethodPost, "/api/v1/players/p1/volume", bytes.NewReader([]byte(`{"volume":80}`))), "id", "p1"))
	if rr.Code != http.StatusOK {
		t.Fatalf("音量状态码 = %d：%s", rr.Code, rr.Body.String())
	}
	var p player.Player
	if err := json.Unmarshal(rr.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode volume: %v", err)
	}
	if p.Volume != 60 {
		t.Fatalf("音量 = %d，期望钳到 60", p.Volume)
	}
}
