package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"songloft/internal/player"
)

// TestUpdateGroupEndpoint PUT /groups/{id}：200 后 GET 返回新名称与图标。
func TestUpdateGroupEndpoint(t *testing.T) {
	h := newPlaybackHandlerForTest(t)
	ctx := context.Background()
	if err := h.mgr.RegisterPlayer(ctx, player.Player{ID: "a", Kind: player.KindRAOP, Name: "客厅"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	gid := h.mgr.GroupIDOf("a")

	body, _ := json.Marshal(map[string]string{"name": "书房", "icon": "bookshelf"})
	rr := httptest.NewRecorder()
	h.UpdateGroup(rr, withURLParams(httptest.NewRequest(http.MethodPut, "/api/v1/groups/"+gid, bytes.NewReader(body)), "id", gid))
	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 = %d：%s", rr.Code, rr.Body.String())
	}
	var updated player.GroupView
	if err := json.Unmarshal(rr.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Name != "书房" || updated.Icon != "bookshelf" {
		t.Fatalf("PUT 返回 name=%q icon=%q", updated.Name, updated.Icon)
	}

	// 验收：PUT 后 GET /groups/{id} 返回新 name/icon。
	rr = httptest.NewRecorder()
	h.GetGroup(rr, withURLParams(httptest.NewRequest(http.MethodGet, "/api/v1/groups/"+gid, nil), "id", gid))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET 状态码 = %d：%s", rr.Code, rr.Body.String())
	}
	var got player.GroupView
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Name != "书房" || got.Icon != "bookshelf" {
		t.Fatalf("GET 返回 name=%q icon=%q，期望 书房 / bookshelf", got.Name, got.Icon)
	}
}

// TestUpdateGroupEndpointValidation 400 / 404 映射。
func TestUpdateGroupEndpointValidation(t *testing.T) {
	h := newPlaybackHandlerForTest(t)
	ctx := context.Background()
	if err := h.mgr.RegisterPlayer(ctx, player.Player{ID: "a", Kind: player.KindRAOP, Name: "客厅"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	gid := h.mgr.GroupIDOf("a")

	cases := []struct {
		name string
		id   string
		body string
		code int
	}{
		{name: "空名", id: gid, body: `{"name":"","icon":""}`, code: http.StatusBadRequest},
		{name: "超长", id: gid, body: `{"name":"` + strings.Repeat("区", 41) + `","icon":""}`, code: http.StatusBadRequest},
		{name: "非法图标", id: gid, body: `{"name":"客厅","icon":"rocket"}`, code: http.StatusBadRequest},
		{name: "请求格式错误", id: gid, body: `{`, code: http.StatusBadRequest},
		{name: "组不存在", id: "grp-missing", body: `{"name":"客厅","icon":""}`, code: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.UpdateGroup(rr, withURLParams(httptest.NewRequest(http.MethodPut, "/api/v1/groups/"+tc.id, bytes.NewBufferString(tc.body)), "id", tc.id))
			if rr.Code != tc.code {
				t.Fatalf("状态码 = %d，期望 %d：%s", rr.Code, tc.code, rr.Body.String())
			}
		})
	}
}
