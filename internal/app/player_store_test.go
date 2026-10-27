package app

import (
	"context"
	"testing"
	"time"

	"songloft/internal/player"
)

// TestSongTrackResolverMetadata A1：resolver 把屏显元数据
// （Artist/Album/ArtworkPath）一并填进 Track。
func TestSongTrackResolverMetadata(t *testing.T) {
	r := songTrackResolver{get: func(ctx context.Context, id int64) (songTrackInfo, error) {
		return songTrackInfo{
			Title:       "夜曲",
			Path:        "/music/夜曲.flac",
			Duration:    253.5,
			Format:      "flac",
			SampleRate:  44100,
			BitRate:     900,
			Artist:      "周杰伦",
			Album:       "十一月的萧邦",
			ArtworkPath: "/music/cover.jpg",
		}, nil
	}}
	track, err := r.ResolveTrack(context.Background(), 7)
	if err != nil {
		t.Fatalf("ResolveTrack: %v", err)
	}
	if track.Artist != "周杰伦" || track.Album != "十一月的萧邦" || track.ArtworkPath != "/music/cover.jpg" {
		t.Fatalf("屏显元数据未透传：Artist=%q Album=%q ArtworkPath=%q",
			track.Artist, track.Album, track.ArtworkPath)
	}
	if track.Title != "夜曲" || track.Duration != time.Duration(253.5*float64(time.Second)) {
		t.Fatalf("基础字段被破坏：Title=%q Duration=%v", track.Title, track.Duration)
	}
}

// TestSongTrackResolverRemoteURL 远程歌曲走 RemoteURL，本地路径为空也放行。
func TestSongTrackResolverRemoteURL(t *testing.T) {
	r := songTrackResolver{get: func(ctx context.Context, id int64) (songTrackInfo, error) {
		return songTrackInfo{Title: "电台", URL: "https://example.com/stream.mp3", Duration: 0}, nil
	}}
	track, err := r.ResolveTrack(context.Background(), 1)
	if err != nil {
		t.Fatalf("ResolveTrack: %v", err)
	}
	if track.RemoteURL != "https://example.com/stream.mp3" {
		t.Fatalf("RemoteURL = %q", track.RemoteURL)
	}
}

var _ player.TrackResolver = songTrackResolver{}
