package app

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"songloft/internal/player/sink"
)

// Stream capabilities authorize one song and format, never the user's account.
type dlnaStreams struct{ key [32]byte }

func newDLNAStreams() *dlnaStreams {
	s := &dlnaStreams{}
	if _, err := rand.Read(s.key[:]); err != nil {
		panic(err)
	}
	return s
}

func (s *dlnaStreams) signature(id, format, expires string) string {
	h := hmac.New(sha256.New, s.key[:])
	fmt.Fprintf(h, "%s\n%s\n%s", id, format, expires)
	return hex.EncodeToString(h.Sum(nil))
}

func (s *dlnaStreams) query(id int64, format string, duration time.Duration) string {
	ttl := duration + time.Hour
	if ttl < 2*time.Hour {
		ttl = 2 * time.Hour
	}
	if ttl > 24*time.Hour {
		ttl = 24 * time.Hour
	}
	expires := strconv.FormatInt(time.Now().Add(ttl).Unix(), 10)
	song := strconv.FormatInt(id, 10)
	return url.Values{"format": {format}, "expires": {expires}, "ticket": {s.signature(song, format, expires)}}.Encode()
}

func (s *dlnaStreams) protect(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		id, format, expires := chi.URLParam(r, "id"), q.Get("format"), q.Get("expires")
		expiry, err := strconv.ParseInt(expires, 10, 64)
		if err != nil || expiry <= time.Now().Unix() || !hmac.Equal([]byte(q.Get("ticket")), []byte(s.signature(id, format, expires))) {
			http.Error(w, "invalid or expired stream ticket", http.StatusForbidden)
			return
		}
		// Only the signed format reaches the media handler; arbitrary transform or
		// proxy parameters from a renderer must not alter this capability.
		cloned := r.Clone(r.Context())
		u := *r.URL
		u.RawQuery = url.Values{"format": {format}}.Encode()
		cloned.URL = &u
		next(w, cloned)
	}
}

func (a *App) dlnaStreamPlan(ctx context.Context, target string, plan sink.StreamPlan) (sink.StreamPlan, error) {
	song, err := a.songService.GetByID(ctx, plan.SongID)
	if err != nil {
		return plan, err
	}
	format := strings.ToLower(song.Format)
	switch format {
	case "wav", "flac", "mp3":
	default:
		format = "mp3"
	}
	plan.Format.Codec = sink.Codec(format)
	u, err := url.Parse(target)
	if err != nil {
		return plan, err
	}
	plan.StreamURL, err = a.lanURL.Resolve(ctx, u.Hostname(), fmt.Sprintf("/api/v1/playback/dlna/stream/%d", plan.SongID), a.dlnaStreams.query(plan.SongID, format, plan.Duration))
	return plan, err
}
