package app

import (
	"context"
	"github.com/go-chi/chi/v5"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestDLNAStreamCapability(t *testing.T) {
	s := newDLNAStreams()
	ticket := s.query(42, "mp3", time.Minute)
	for _, tc := range []struct {
		name, id, query string
		want            int
	}{
		{"valid", "42", ticket, 200},
		{"different song", "43", ticket, 403},
		{"missing ticket", "42", "format=mp3", 403},
		{"transform tamper", "42", "format=wav&" + ticket, 403},
		{"extra query stripped", "42", ticket + "&url=http://example.com&seek=90", 200},
		{"expired", "42", url.Values{"format": {"mp3"}, "expires": {"1"}, "ticket": {s.signature("42", "mp3", "1")}}.Encode(), 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/stream?"+tc.query, nil)
			route := chi.NewRouteContext()
			route.URLParams.Add("id", tc.id)
			req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, route))
			rec := httptest.NewRecorder()
			s.protect(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.RawQuery != "format=mp3" {
					t.Errorf("unexpected params %q", r.URL.RawQuery)
				}
				w.WriteHeader(200)
			})(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status %d want %d", rec.Code, tc.want)
			}
		})
	}
}
