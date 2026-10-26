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

// TestSleepTimerEndpoints POST 1 分钟后 GET 返回约 60 秒；POST 0 后 active=false。
func TestSleepTimerEndpoints(t *testing.T) {
	h := newPlaybackHandlerForTest(t)
	ctx := context.Background()
	if err := h.mgr.RegisterPlayer(ctx, player.Player{ID: "a", Kind: player.KindRAOP, Name: "客厅"}); err != nil {
		t.Fatalf("register: %v", err)
	}
	gid := h.mgr.GroupIDOf("a")
	defer func() { _ = h.mgr.SetSleepTimer(gid, 0) }()

	post := func(id, body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.SetSleepTimer(rr, withURLParams(
			httptest.NewRequest(http.MethodPost, "/api/v1/groups/"+id+"/sleep", bytes.NewBufferString(body)),
			"id", id,
		))
		return rr
	}
	get := func(id string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		h.GetSleepTimer(rr, withURLParams(
			httptest.NewRequest(http.MethodGet, "/api/v1/groups/"+id+"/sleep", nil),
			"id", id,
		))
		return rr
	}
	decode := func(t *testing.T, rr *httptest.ResponseRecorder) sleepTimerStatus {
		t.Helper()
		var st sleepTimerStatus
		if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
			t.Fatalf("解析响应: %v（%s）", err, rr.Body.String())
		}
		return st
	}

	rr := post(gid, `{"minutes":1}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST 状态码 = %d：%s", rr.Code, rr.Body.String())
	}
	st := decode(t, rr)
	if !st.Active || st.RemainingSeconds < 55 || st.RemainingSeconds > 60 {
		t.Fatalf("POST 返回 active=%v remaining=%d，期望 active=true 且约 60 秒", st.Active, st.RemainingSeconds)
	}

	rr = get(gid)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET 状态码 = %d：%s", rr.Code, rr.Body.String())
	}
	st = decode(t, rr)
	if !st.Active || st.RemainingSeconds < 55 || st.RemainingSeconds > 60 {
		t.Fatalf("GET 返回 active=%v remaining=%d，期望 active=true 且约 60 秒", st.Active, st.RemainingSeconds)
	}

	rr = post(gid, `{"minutes":0}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("取消状态码 = %d：%s", rr.Code, rr.Body.String())
	}
	st = decode(t, rr)
	if st.Active || st.RemainingSeconds != 0 {
		t.Fatalf("取消后返回 active=%v remaining=%d，期望 false/0", st.Active, st.RemainingSeconds)
	}

	rr = get(gid)
	if rr.Code != http.StatusOK {
		t.Fatalf("取消后 GET 状态码 = %d：%s", rr.Code, rr.Body.String())
	}
	st = decode(t, rr)
	if st.Active || st.RemainingSeconds != 0 {
		t.Fatalf("取消后 GET 返回 active=%v remaining=%d，期望 false/0", st.Active, st.RemainingSeconds)
	}
}

// TestSleepTimerValidation 非法 minutes 返回 400，不存在的组返回 404。
func TestSleepTimerValidation(t *testing.T) {
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
		{name: "负数", id: gid, body: `{"minutes":-1}`, code: http.StatusBadRequest},
		{name: "超过上限", id: gid, body: `{"minutes":721}`, code: http.StatusBadRequest},
		{name: "请求格式错误", id: gid, body: `{`, code: http.StatusBadRequest},
		{name: "组不存在", id: "grp-missing", body: `{"minutes":1}`, code: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			h.SetSleepTimer(rr, withURLParams(
				httptest.NewRequest(http.MethodPost, "/api/v1/groups/"+tc.id+"/sleep", bytes.NewBufferString(tc.body)),
				"id", tc.id,
			))
			if rr.Code != tc.code {
				t.Fatalf("状态码 = %d，期望 %d：%s", rr.Code, tc.code, rr.Body.String())
			}
		})
	}

	rr := httptest.NewRecorder()
	h.GetSleepTimer(rr, withURLParams(
		httptest.NewRequest(http.MethodGet, "/api/v1/groups/grp-missing/sleep", nil),
		"id", "grp-missing",
	))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET 不存在组状态码 = %d，期望 404：%s", rr.Code, rr.Body.String())
	}
}
