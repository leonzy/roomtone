package ws

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"songloft/internal/player"
)

func newTestHub() *Hub {
	return NewHub(player.NewEventBus(), slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestHubTicketAuth 一次性 ticket 是 WS 唯一的鉴权手段：浏览器无法给握手设置
// Authorization 头，所以这里必须自己挡住无票 / 假票 / 过期票 / 重复用票。
func TestHubTicketAuth(t *testing.T) {
	h := newTestHub()
	srv := httptest.NewServer(h)
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/playback/events"

	dial := func(query string) (int, error) {
		c, resp, err := websocket.DefaultDialer.Dial(wsURL+query, nil)
		if err != nil {
			if resp != nil {
				return resp.StatusCode, err
			}
			return 0, err
		}
		_ = c.Close()
		return http.StatusSwitchingProtocols, nil
	}

	if code, _ := dial(""); code != http.StatusUnauthorized {
		t.Fatalf("无 ticket 升级状态 = %d, want 401", code)
	}
	if code, _ := dial("?ticket=forged"); code != http.StatusUnauthorized {
		t.Fatalf("伪造 ticket 升级状态 = %d, want 401", code)
	}

	token, _, err := h.Tickets().Issue("user-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if code, err := dial("?ticket=" + token); err != nil || code != http.StatusSwitchingProtocols {
		t.Fatalf("有效 ticket 升级 = %d, err=%v, want 101", code, err)
	}
	// 用后即焚。
	if code, _ := dial("?ticket=" + token); code != http.StatusUnauthorized {
		t.Fatalf("重复使用 ticket 升级状态 = %d, want 401", code)
	}
}

// TestHubTicketExpiry 过期 ticket 必须失效，避免 URL 泄露后被长期复用。
func TestHubTicketExpiry(t *testing.T) {
	store := NewTicketStore()
	now := time.Now()
	store.SetClock(func() time.Time { return now })

	token, _, err := store.Issue("user-1")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	now = now.Add(TicketTTL + time.Second)
	if _, err := store.Redeem(token); err == nil {
		t.Fatal("过期 ticket 不应兑换成功")
	}
}
