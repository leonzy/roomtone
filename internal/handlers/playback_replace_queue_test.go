package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"songloft/internal/player"
)

func putQueue(t *testing.T, h *PlaybackHandler, groupID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(
		http.MethodPut,
		"/api/v1/groups/"+groupID+"/queue",
		strings.NewReader(body),
	)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", groupID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	h.ReplaceQueue(rr, req)
	return rr
}

// TestReplaceQueueOverwritesWholeQueue 客户端推队列要整条覆盖，不是追加。
func TestReplaceQueueOverwritesWholeQueue(t *testing.T) {
	h, e, gid := newQueueTestHandler(t)
	e.Enqueue(1, 2, 3)

	rr := putQueue(t, h, gid, `{"song_ids":[7,8],"index":1}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200：%s", rr.Code, rr.Body.String())
	}
	var st player.Status
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if len(st.Queue.Items) != 2 || st.Queue.Items[0] != 7 || st.Queue.Items[1] != 8 {
		t.Fatalf("队列 = %v，期望 [7 8]", st.Queue.Items)
	}
	if st.Queue.Current != 1 {
		t.Fatalf("current = %d，期望 1", st.Queue.Current)
	}
	if st.State != player.StateIdle {
		t.Fatalf("state = %s，期望 idle（推队列不能起播）", st.State)
	}
}

// TestReplaceQueueEmptyClears 空数组表示清空队列。
func TestReplaceQueueEmptyClears(t *testing.T) {
	h, e, gid := newQueueTestHandler(t)
	e.Enqueue(1, 2)

	rr := putQueue(t, h, gid, `{"song_ids":[],"index":0}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200：%s", rr.Code, rr.Body.String())
	}
	var st player.Status
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if len(st.Queue.Items) != 0 {
		t.Fatalf("队列 = %v，期望空", st.Queue.Items)
	}
}

// TestReplaceQueueRejectsBadJSON 请求体不合法必须 400。
func TestReplaceQueueRejectsBadJSON(t *testing.T) {
	h, _, gid := newQueueTestHandler(t)

	rr := putQueue(t, h, gid, `{"song_ids":`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400", rr.Code)
	}
}

// TestReplaceQueueUnknownGroup 组不存在必须 404。
func TestReplaceQueueUnknownGroup(t *testing.T) {
	h, _, _ := newQueueTestHandler(t)

	rr := putQueue(t, h, "grp-missing", `{"song_ids":[1],"index":0}`)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("状态码 = %d，期望 404：%s", rr.Code, rr.Body.String())
	}
}
