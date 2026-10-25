package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"songloft/internal/player"
)

// B6-05：预设端点必须给出可直接 PUT 回去的曲线（10 段 + preamp）。
func TestListEQPresets(t *testing.T) {
	h := newPlaybackHandlerForTest(t)
	if err := h.mgr.RegisterPlayer(context.Background(), player.Player{ID: "p1", Kind: player.KindRAOP, Name: "客厅"}); err != nil {
		t.Fatalf("register: %v", err)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/players/p1/eq/presets", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "p1")
	h.ListEQPresets(rr, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx)))

	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rr.Code, rr.Body.String())
	}
	var got []player.EQPreset
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(player.EQPresets()) {
		t.Fatalf("预设条数 %d，期望 %d", len(got), len(player.EQPresets()))
	}
	for _, p := range got {
		if p.ID == "" || p.Name == "" || len(p.Bands) != player.EQBandCount {
			t.Fatalf("预设不完整：%+v", p)
		}
	}
}

// 设备不存在要 404，不能拿"全局预设"当借口返回 200。
func TestListEQPresetsUnknownPlayer(t *testing.T) {
	h := newPlaybackHandlerForTest(t)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/players/nope/eq/presets", nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "nope")
	h.ListEQPresets(rr, req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx)))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404", rr.Code)
	}
}
