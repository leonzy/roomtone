package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"songloft/internal/player"
)

func TestUpdatePlayerPasswordSetPreserveAndClear(t *testing.T) {
	h := newPlaybackHandlerForTest(t)
	if err := h.mgr.RegisterPlayer(context.Background(), player.Player{ID: "p1", Kind: player.KindRAOP}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ body, want string }{
		{`{"password":"speaker-test-secret"}`, "speaker-test-secret"},
		{`{"name":"Room"}`, "speaker-test-secret"},
		{`{"password":""}`, ""},
	} {
		rr := httptest.NewRecorder()
		h.UpdatePlayer(rr, withURLParams(httptest.NewRequest(http.MethodPut, "/api/v1/players/p1", bytes.NewBufferString(tc.body)), "id", "p1"))
		if rr.Code != http.StatusOK {
			t.Fatalf("update: %d %s", rr.Code, rr.Body.String())
		}
		p, err := h.mgr.GetPlayer("p1")
		if err != nil {
			t.Fatal(err)
		}
		if p.Password != tc.want {
			t.Fatal("password update/preservation failed")
		}
		if bytes.Contains(rr.Body.Bytes(), []byte("speaker-test-secret")) || bytes.Contains(rr.Body.Bytes(), []byte(`"password"`)) {
			t.Fatal("password exposed in player response")
		}
	}
}
