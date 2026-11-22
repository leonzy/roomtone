package handlers

import (
	"context"
	"net/http"
)

type dlnaPlaybackKey struct{}

// WithDLNAPlayback is applied only after the song-scoped stream ticket is
// verified. Renderers receive a complete, seekable representation, including
// for cold transcodes; browser playback keeps its existing live-stream path.
func WithDLNAPlayback(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), dlnaPlaybackKey{}, true))
}

type dlnaChannelKey struct{}

// WithDLNAChannel carries only the route verified by the signed stream ticket.
func WithDLNAChannel(r *http.Request, mode string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), dlnaChannelKey{}, mode))
}
func dlnaChannel(r *http.Request) string {
	mode, _ := r.Context().Value(dlnaChannelKey{}).(string)
	return mode
}
