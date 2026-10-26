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

// newQueueTestHandler 建一个只含本机单机组的 manager，返回 handler 与组 id。
func newQueueTestHandler(t *testing.T) (*PlaybackHandler, *player.Engine, string) {
	t.Helper()
	mgr := player.NewManager(player.ManagerDeps{})
	ctx := context.Background()
	if err := mgr.RegisterPlayer(ctx, player.Player{
		ID: "local-1", Kind: player.KindLocal, Name: "本机",
	}); err != nil {
		t.Fatalf("register player: %v", err)
	}
	gid := mgr.GroupIDOf("local-1")
	if gid == "" {
		t.Fatal("本机没有自动建单机组")
	}
	e, err := mgr.Engine(gid)
	if err != nil {
		t.Fatalf("engine: %v", err)
	}
	return NewPlaybackHandler(mgr, nil), e, gid
}

func postQueue(t *testing.T, h *PlaybackHandler, groupID, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/groups/"+groupID+"/queue",
		strings.NewReader(body),
	)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", groupID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	h.AddToQueue(rr, req)
	return rr
}

// TestAddToQueuePositionNext 下一首播放必须插到当前曲之后，而不是队尾。
func TestAddToQueuePositionNext(t *testing.T) {
	h, e, gid := newQueueTestHandler(t)
	e.Enqueue(1, 2, 3)

	rr := postQueue(t, h, gid, `{"song_ids":[9,10],"position":"next"}`)
	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200：%s", rr.Code, rr.Body.String())
	}
	var st player.Status
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	want := []int64{1, 9, 10, 2, 3}
	if len(st.Queue.Items) != len(want) {
		t.Fatalf("队列 = %v，期望 %v", st.Queue.Items, want)
	}
	for i := range want {
		if st.Queue.Items[i] != want[i] {
			t.Fatalf("队列 = %v，期望 %v", st.Queue.Items, want)
		}
	}
}

// TestAddToQueueRejectsUnknownPosition 未知 position 必须 400，不能静默当追加。
func TestAddToQueueRejectsUnknownPosition(t *testing.T) {
	h, e, gid := newQueueTestHandler(t)
	e.Enqueue(1, 2)

	rr := postQueue(t, h, gid, `{"song_ids":[9],"position":"start"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("状态码 = %d，期望 400：%s", rr.Code, rr.Body.String())
	}
	if items := e.Status().Queue.Items; len(items) != 2 {
		t.Fatalf("非法 position 不应改队列，实际 %v", items)
	}
}
