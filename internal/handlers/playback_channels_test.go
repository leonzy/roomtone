package handlers

import (
	"context"
	"encoding/json"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/http/httptest"
	"songloft/internal/player"
	"strings"
	"testing"
)

func TestMemberChannelPatchPreservesOtherSettings(t *testing.T) {
	h := newPlaybackHandlerForTest(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if err := h.mgr.RegisterPlayer(ctx, player.Player{ID: id, Name: id, Kind: player.KindRAOP}); err != nil {
			t.Fatal(err)
		}
	}
	g, err := h.mgr.CreateGroup(ctx, "pair", []string{"a", "b"}, "a")
	if err != nil {
		t.Fatal(err)
	}
	if g.Members[0].ChannelMode != "stereo" || g.Members[1].ChannelMode != "stereo" {
		t.Fatal("new group is not stereo by default")
	}
	if _, err := h.mgr.UpdateMember(ctx, g.ID, "a", 120, 4); err != nil {
		t.Fatal(err)
	}
	patch := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(body))
		rc := chi.NewRouteContext()
		rc.URLParams.Add("id", g.ID)
		rc.URLParams.Add("player_id", "a")
		h.UpdateMember(rr, req.WithContext(context.WithValue(ctx, chi.RouteCtxKey, rc)))
		return rr
	}
	rr := patch(`{"channel_mode":"left"}`)
	if rr.Code != 200 {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
	var got player.GroupView
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Members[0].DelayMs != 120 || got.Members[0].VolumeOffset != 4 || got.Members[0].ChannelMode != "left" {
		t.Fatal(got.Members[0])
	}
	rr = patch(`{"volume_offset":8}`)
	if rr.Code != 200 {
		t.Fatal(rr.Body.String())
	}
	got, _ = h.mgr.GetGroup(g.ID)
	if got.Members[0].ChannelMode != "left" || got.Members[0].DelayMs != 120 {
		t.Fatal(got.Members[0])
	}
	if patch(`{"channel_mode":"back_center"}`).Code != 200 {
		t.Fatal("DTS-ES back center rejected")
	}
	if patch(`{"channel_mode":"surround"}`).Code != 400 {
		t.Fatal("invalid mode accepted")
	}
}
