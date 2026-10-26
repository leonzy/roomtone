package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"songloft/internal/models"
	"songloft/internal/services"
)

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func newBatchSongHandler(t *testing.T) (*SongHandler, []int64) {
	t.Helper()
	repo := newTestSongRepo(t)
	ids := []int64{
		seedSong(t, repo, &models.Song{Type: models.TypeLocal, Title: "一", FilePath: "/music/1.mp3"}),
		seedSong(t, repo, &models.Song{Type: models.TypeLocal, Title: "二", FilePath: "/music/2.mp3"}),
	}
	svc := services.NewSongService(repo, nil, nil, nil, nil, nil)
	return NewSongHandler(svc, nil, nil, nil, nil, nil), ids
}

func getBatch(t *testing.T, h *SongHandler, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/songs/batch?"+query, nil)
	rr := httptest.NewRecorder()
	h.BatchGetSongs(rr, req)
	return rr
}

// TestBatchGetSongsReturnsInRequestOrder 批量取歌要按请求顺序返回，
// 客户端拿它还原区域队列，顺序错了队列就乱了。
func TestBatchGetSongsReturnsInRequestOrder(t *testing.T) {
	h, ids := newBatchSongHandler(t)

	rr := getBatch(t, h, "ids="+itoa(ids[1])+","+itoa(ids[0]))
	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200：%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Songs []*models.Song `json:"songs"`
		Total int            `json:"total"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if body.Total != 2 || len(body.Songs) != 2 {
		t.Fatalf("total = %d, len = %d，期望 2", body.Total, len(body.Songs))
	}
	if body.Songs[0].ID != ids[1] || body.Songs[1].ID != ids[0] {
		t.Fatalf("顺序 = [%d %d]，期望 [%d %d]",
			body.Songs[0].ID, body.Songs[1].ID, ids[1], ids[0])
	}
}

// TestBatchGetSongsSkipsMissing 不存在的 id 直接跳过，不报错：
// 队列里可能有已被删除的歌，不能因此整条队列取不回来。
func TestBatchGetSongsSkipsMissing(t *testing.T) {
	h, ids := newBatchSongHandler(t)

	rr := getBatch(t, h, "ids="+itoa(ids[0])+",999999")
	if rr.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200：%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Songs []*models.Song `json:"songs"`
		Total int            `json:"total"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if body.Total != 1 || len(body.Songs) != 1 || body.Songs[0].ID != ids[0] {
		t.Fatalf("返回 = %+v，期望只含 %d", body.Songs, ids[0])
	}
}

// TestBatchGetSongsRejectsBadInput 空 ids / 非法 id / 超过 500 个都要 400。
func TestBatchGetSongsRejectsBadInput(t *testing.T) {
	h, ids := newBatchSongHandler(t)

	cases := []string{
		"",
		"ids=",
		"ids=abc",
		"ids=0",
		"ids=-1",
		"ids=" + itoa(ids[0]) + ",x",
		"ids=" + strings.Repeat("1,", 500) + "1",
	}
	for _, q := range cases {
		if rr := getBatch(t, h, q); rr.Code != http.StatusBadRequest {
			t.Fatalf("query %q 状态码 = %d，期望 400", q, rr.Code)
		}
	}
}
