package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"songloft/internal/player"
)

func postModeTransport(h *PlaybackHandler, id, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/groups/"+id+"/transport", strings.NewReader(body))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	h.Transport(rr, req)
	return rr
}

func TestTransportCombinedQueueMode(t *testing.T) {
	for _, mode := range []struct {
		repeat  player.RepeatMode
		shuffle bool
	}{
		{player.RepeatOff, false}, {player.RepeatAll, false}, {player.RepeatOne, false}, {player.RepeatOff, true},
	} {
		t.Run(string(mode.repeat)+"/shuffle="+map[bool]string{true: "true", false: "false"}[mode.shuffle], func(t *testing.T) {
			h, engine, id := newQueueTestHandler(t)
			t.Cleanup(engine.Stop)
			if err := engine.SetQueueWithProgress([]int64{1, 2, 3}, 1, []int{0, 1}); err != nil {
				t.Fatal(err)
			}
			before := engine.Status()
			body, _ := json.Marshal(map[string]any{"action": "", "repeat": mode.repeat, "shuffle": mode.shuffle})
			rr := postModeTransport(h, id, string(body))
			if rr.Code != http.StatusOK {
				t.Fatalf("combined mode rejected: %d %s", rr.Code, rr.Body.String())
			}
			after := engine.Status()
			if after.Queue.Repeat != mode.repeat || after.Queue.Shuffle != mode.shuffle {
				t.Fatalf("mode did not change: %+v", after.Queue)
			}
			if after.SongID != before.SongID || after.Position != before.Position || after.State != before.State || after.Queue.Current != before.Queue.Current || !reflect.DeepEqual(after.Queue.Items, before.Queue.Items) || !reflect.DeepEqual(after.Queue.Started, before.Queue.Started) {
				t.Fatalf("mode changed playback/queue: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestTransportModeValidationDoesNotPartiallyApply(t *testing.T) {
	for _, body := range []string{`{}`, `{"action":"unknown","repeat":"off","shuffle":true}`, `{"repeat":"invalid","shuffle":true}`, `{"repeat":"one","shuffle":"yes"}`} {
		h, engine, id := newQueueTestHandler(t)
		t.Cleanup(engine.Stop)
		engine.SetRepeat(player.RepeatAll)
		before := engine.Status()
		rr := postModeTransport(h, id, body)
		if rr.Code != http.StatusBadRequest || !reflect.DeepEqual(before, engine.Status()) {
			t.Fatalf("invalid patch %s: code=%d, state changed=%v", body, rr.Code, !reflect.DeepEqual(before, engine.Status()))
		}
	}
}

func TestTransportModePartialAndExplicitActions(t *testing.T) {
	h, engine, id := newQueueTestHandler(t)
	t.Cleanup(engine.Stop)
	for _, body := range []string{`{"action":"repeat","repeat":"all"}`, `{"shuffle":true}`, `{"repeat":"one"}`, `{"action":"shuffle","shuffle":false}`} {
		if rr := postModeTransport(h, id, body); rr.Code != http.StatusOK {
			t.Fatalf("mode request %s rejected: %s", body, rr.Body.String())
		}
	}
	if s := engine.Status(); s.Queue.Repeat != player.RepeatOne || s.Queue.Shuffle {
		t.Fatalf("partial updates lost mode: %+v", s.Queue)
	}
}
