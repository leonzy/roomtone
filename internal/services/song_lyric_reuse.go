package services

import (
	"context"
	"math"
	"songloft/internal/database"
	"songloft/internal/models"
)

// ExistingAudioLyric only reuses a complete exact fingerprint with a matching duration.
// Different performances sharing a title must never inherit each other's timing.
func (s *SongService) ExistingAudioLyric(ctx context.Context, song *models.Song) *models.LyricPayload {
	if len(song.Fingerprint) < 100 || song.Duration <= 0 {
		return nil
	}
	candidates, err := s.songs.List(ctx, &database.SongFilter{Fingerprint: song.Fingerprint, WithLyrics: true, Limit: 8})
	if err != nil {
		return nil
	}
	for _, candidate := range candidates {
		if candidate.ID == song.ID || math.Abs(candidate.Duration-song.Duration) > 1 {
			continue
		}
		payload := models.UnmarshalLyric(candidate.Lyric)
		if !payload.IsEmpty() {
			return &payload
		}
	}
	return nil
}
