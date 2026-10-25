package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"songloft/internal/player"
)

// withURLParams 给 handler 单测塞路径参数。
func withURLParams(req *http.Request, kv ...string) *http.Request {
	rctx := chi.NewRouteContext()
	for i := 0; i+1 < len(kv); i += 2 {
		rctx.URLParams.Add(kv[i], kv[i+1])
	}
	return req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
}

// TestDeletePlayerEndpoint 删除设备返回 200，再删 404。
func TestDeletePlayerEndpoint(t *testing.T) {
	h := newPlaybackHandlerForTest(t)
	if err := h.mgr.RegisterPlayer(context.Background(), player.Player{ID: "p1", Kind: player.KindRAOP, Name: "客厅"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	rr := httptest.NewRecorder()
	h.DeletePlayer(rr, withURLParams(httptest.NewRequest(http.MethodDelete, "/api/v1/players/p1", nil), "id", "p1"))
	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	h.DeletePlayer(rr, withURLParams(httptest.NewRequest(http.MethodDelete, "/api/v1/players/p1", nil), "id", "p1"))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("重复删除状态码 = %d，期望 404", rr.Code)
	}
}

// TestDeleteGroupEndpoint 删组后成员仍在（各自单机组），删不存在的组 404。
func TestDeleteGroupEndpoint(t *testing.T) {
	h := newPlaybackHandlerForTest(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if err := h.mgr.RegisterPlayer(ctx, player.Player{ID: id, Kind: player.KindRAOP, Name: id}); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	ga := h.mgr.GroupIDOf("a")
	if _, err := h.mgr.AddMember(ctx, ga, "b"); err != nil {
		t.Fatalf("add: %v", err)
	}
	rr := httptest.NewRecorder()
	h.DeleteGroup(rr, withURLParams(httptest.NewRequest(http.MethodDelete, "/api/v1/groups/x", nil), "id", ga))
	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rr.Code, rr.Body.String())
	}
	if n := len(h.mgr.ListPlayers()); n != 2 {
		t.Fatalf("删组不该删设备，剩 %d 台", n)
	}
	rr = httptest.NewRecorder()
	h.DeleteGroup(rr, withURLParams(httptest.NewRequest(http.MethodDelete, "/api/v1/groups/x", nil), "id", ga))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("删不存在的组状态码 = %d，期望 404", rr.Code)
	}
}

// TestSetMasterEndpoint 换主控成功，缺 player_id 400。
func TestSetMasterEndpoint(t *testing.T) {
	h := newPlaybackHandlerForTest(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if err := h.mgr.RegisterPlayer(ctx, player.Player{ID: id, Kind: player.KindRAOP, Name: id}); err != nil {
			t.Fatalf("register: %v", err)
		}
	}
	ga := h.mgr.GroupIDOf("a")
	if _, err := h.mgr.AddMember(ctx, ga, "b"); err != nil {
		t.Fatalf("add: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"player_id": "b"})
	rr := httptest.NewRecorder()
	h.SetMaster(rr, withURLParams(httptest.NewRequest(http.MethodPost, "/api/v1/groups/x/master", bytes.NewReader(body)), "id", ga))
	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rr.Code, rr.Body.String())
	}
	var g player.GroupView
	if err := json.Unmarshal(rr.Body.Bytes(), &g); err != nil {
		t.Fatal(err)
	}
	if g.MasterID != "b" {
		t.Fatalf("主控 = %q，期望 b", g.MasterID)
	}
	rr = httptest.NewRecorder()
	h.SetMaster(rr, withURLParams(httptest.NewRequest(http.MethodPost, "/api/v1/groups/x/master", bytes.NewReader([]byte(`{}`))), "id", ga))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("空 player_id 状态码 = %d，期望 400", rr.Code)
	}
}
