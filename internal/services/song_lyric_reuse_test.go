package services

import (
	"context"
	"songloft/internal/models"
	"strings"
	"testing"
)

func TestExistingAudioLyric(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fingerprint string
		duration    float64
		want        bool
	}{
		{"exact audio", strings.Repeat("fingerprint", 30), 297, true},
		{"different duration", strings.Repeat("fingerprint", 30), 310, false},
		{"different fingerprint", strings.Repeat("other", 60), 297, false},
		{"missing fingerprint", "", 297, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newTestSongRepo(t)
			ctx := context.Background()
			known := &models.Song{Type: models.TypeLocal, Title: "Known", FilePath: "/known.flac", Duration: 297, Fingerprint: strings.Repeat("fingerprint", 30), Lyric: "[00:01]existing", LyricSource: models.LyricSourceFile}
			if err := repo.Create(ctx, known); err != nil {
				t.Fatal(err)
			}
			service := NewSongService(repo, nil, nil, nil, nil, nil)
			got := service.ExistingAudioLyric(ctx, &models.Song{ID: known.ID + 1, Title: "Untrusted filename", Duration: tc.duration, Fingerprint: tc.fingerprint})
			if (got != nil) != tc.want {
				t.Fatalf("got %v, want found=%v", got, tc.want)
			}
		})
	}
}
