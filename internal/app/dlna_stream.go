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
	"songloft/internal/handlers"
	"songloft/internal/models"
	"songloft/internal/player/sink"
	"songloft/internal/player/sink/dlna"
	"songloft/internal/services"
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
		if strings.HasPrefix(format, "original:") {
			u.RawQuery = ""
		} else {
			u.RawQuery = url.Values{"format": {format}}.Encode()
		}
		cloned.URL = &u
		cloned = handlers.WithDLNAPlayback(cloned)
		w.Header().Set("transferMode.dlna.org", "Streaming")
		next(w, cloned)
	}
}

func (a *App) dlnaStreamPlan(ctx context.Context, target string, plan sink.StreamPlan) (sink.StreamPlan, error) {
	song, err := a.songService.GetByID(ctx, plan.SongID)
	if err != nil {
		return plan, err
	}
	sourceFormat := services.EffectiveSourceFormat(song, song.FilePath)
	if sourceFormat == "" {
		sourceFormat = "unknown"
	}
	format := string(dlna.SelectCodec(sourceFormat, plan.AcceptedCodecs))
	if format == "" {
		return plan, fmt.Errorf("dlna: renderer advertises no supported output codec")
	}
	plan.Title, plan.Artist, plan.Album = song.Title, song.Artist, song.Album
	normalize := a.configService.GetBool("volume_normalize", false)
	if normalize || song.CueSourcePath != "" {
		switch format {
		case "flac", "wav", "mp3", "m4a", "ogg":
		default:
			format = string(dlna.SelectCodec("flac", plan.AcceptedCodecs))
			if format == "" {
				return plan, fmt.Errorf("dlna: renderer has no supported processed output codec")
			}
		}
	}
	native := strings.EqualFold(format, sourceFormat) && !normalize && song.CueSourcePath == ""
	// Prepare before SetAVTransportURI/Play so a slow conversion does not leave
	// the renderer waiting on an HTTP response, and errors reach the zone UI.
	if song.Type != models.TypeRadio && !song.IsLive {
		source := song.FilePath
		if song.Type == models.TypeRemote {
			source, err = a.cacheService.Get(ctx, song)
			if err != nil {
				return plan, fmt.Errorf("dlna: prepare source: %w", err)
			}
		}
		if !native {
			if _, err := a.cacheService.GetOrTranscode(ctx, source, song, format, 0, -1, normalize); err != nil {
				return plan, fmt.Errorf("dlna: prepare %s output: %w", format, err)
			}
		}
	}
	plan.Format.Codec = sink.Codec(format)
	u, err := url.Parse(target)
	if err != nil {
		return plan, err
	}
	ticketFormat := format
	if native {
		ticketFormat = "original:" + sourceFormat
	}
	plan.StreamURL, err = a.lanURL.Resolve(ctx, u.Hostname(), fmt.Sprintf("/api/v1/playback/dlna/stream/%d", plan.SongID), a.dlnaStreams.query(plan.SongID, ticketFormat, plan.Duration))
	return plan, err
}
