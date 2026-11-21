package services

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"
	"time"

	"songloft/internal/database"
	"songloft/internal/database/sqlc"
	"songloft/internal/models"
)

type ListeningStart struct {
	SessionID   string
	SongID      int64
	ZoneID      string
	ContextType string
	ContextKey  string
	ClientID    string
	Source      string
	StartedAt   time.Time
}

type ListeningEntry struct {
	ID              int64        `json:"event_id"`
	PlaySessionID   string       `json:"play_session_id"`
	Song            *models.Song `json:"song"`
	ZoneID          string       `json:"zone_id,omitempty"`
	ContextType     string       `json:"context_type,omitempty"`
	ContextKey      string       `json:"context_key,omitempty"`
	Source          string       `json:"source"`
	StartedAt       time.Time    `json:"started_at"`
	EndedAt         *time.Time   `json:"ended_at,omitempty"`
	PlayedSeconds   float64      `json:"played_seconds"`
	CompletionRatio float64      `json:"completion_ratio"`
}

type ListeningHistoryPage struct {
	Items      []ListeningEntry `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
}

type ListeningHistoryService struct {
	db  database.DB
	now func() time.Time
}

func NewListeningHistoryService(db database.DB) *ListeningHistoryService {
	return &ListeningHistoryService{db: db, now: time.Now}
}

func (s *ListeningHistoryService) Start(ctx context.Context, in ListeningStart) (string, error) {
	if in.SongID <= 0 {
		return "", models.ErrMissingSong
	}
	if in.StartedAt.IsZero() {
		in.StartedAt = s.now()
	}
	if in.Source == "" {
		in.Source = "other"
	}
	if in.SessionID == "" {
		existing, err := s.db.ListeningEventRepository().FindRecentOpen(ctx, sqlc.FindRecentOpenListeningEventParams{
			SongID: in.SongID, ZoneID: in.ZoneID, StartedAt: in.StartedAt.Add(-10 * time.Second),
		})
		if err == nil {
			return existing.PlaySessionID, nil
		}
		if !errors.Is(err, database.ErrNotFound) {
			return "", err
		}
		in.SessionID = newPlaySessionID()
	}
	err := s.db.ListeningEventRepository().Start(ctx, sqlc.StartListeningEventParams{
		PlaySessionID: in.SessionID, SongID: in.SongID, ZoneID: in.ZoneID,
		ContextType: in.ContextType, ContextKey: in.ContextKey, ClientID: in.ClientID,
		Source: in.Source, StartedAt: in.StartedAt,
	})
	return in.SessionID, err
}

func (s *ListeningHistoryService) Finish(ctx context.Context, sessionID string, playedSeconds, duration float64, endedAt time.Time) error {
	event, err := s.db.ListeningEventRepository().GetBySession(ctx, sessionID)
	if errors.Is(err, database.ErrNotFound) || event.EndedAt.Valid {
		return nil
	}
	if err != nil {
		return err
	}
	return s.finishEvent(ctx, event, playedSeconds, duration, endedAt)
}

func (s *ListeningHistoryService) FinishLatest(ctx context.Context, songID int64, source string, playedSeconds, duration float64, endedAt time.Time) error {
	if source == "" {
		source = "other"
	}
	return s.db.ListeningEventRepository().FinishLatest(ctx, songID, source, endedAt, playedSeconds, completionRatio(playedSeconds, duration))
}

func (s *ListeningHistoryService) finishEvent(ctx context.Context, event sqlc.ListeningEvent, playedSeconds, duration float64, endedAt time.Time) error {
	if endedAt.IsZero() {
		endedAt = s.now()
	}
	if playedSeconds <= 0 {
		playedSeconds = endedAt.Sub(event.StartedAt).Seconds()
	}
	if playedSeconds < 0 {
		playedSeconds = 0
	}
	return s.db.ListeningEventRepository().Finish(ctx, sqlc.FinishListeningEventParams{
		PlaySessionID: event.PlaySessionID, EndedAt: sql.NullTime{Time: endedAt, Valid: true},
		PlayedSeconds: playedSeconds, CompletionRatio: completionRatio(playedSeconds, duration),
	})
}

func completionRatio(playedSeconds, duration float64) float64 {
	if duration <= 0 {
		return 0
	}
	ratio := playedSeconds / duration
	if ratio > 1 {
		return 1
	}
	if ratio < 0 {
		return 0
	}
	return ratio
}

func (s *ListeningHistoryService) List(ctx context.Context, filter database.ListeningEventFilter) (ListeningHistoryPage, error) {
	if filter.Limit <= 0 || filter.Limit > 100 {
		filter.Limit = 50
	}
	rows, err := s.db.ListeningEventRepository().List(ctx, filter)
	if err != nil {
		return ListeningHistoryPage{}, err
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.SongID)
	}
	songs, err := s.db.SongRepository().ListByIDs(ctx, ids)
	if err != nil {
		return ListeningHistoryPage{}, err
	}
	byID := make(map[int64]*models.Song, len(songs))
	for _, song := range songs {
		byID[song.ID] = song
	}
	page := ListeningHistoryPage{Items: make([]ListeningEntry, 0, len(rows))}
	for _, row := range rows {
		song := byID[row.SongID]
		if song == nil {
			continue
		}
		entry := ListeningEntry{ID: row.ID, PlaySessionID: row.PlaySessionID, Song: song,
			ZoneID: row.ZoneID, ContextType: row.ContextType, ContextKey: row.ContextKey,
			Source: row.Source, StartedAt: row.StartedAt, PlayedSeconds: row.PlayedSeconds,
			CompletionRatio: row.CompletionRatio}
		if row.EndedAt.Valid {
			endedAt := row.EndedAt.Time
			entry.EndedAt = &endedAt
		}
		page.Items = append(page.Items, entry)
	}
	if len(rows) == filter.Limit {
		page.NextCursor = strconv.FormatInt(rows[len(rows)-1].ID, 10)
	}
	return page, nil
}

func (s *ListeningHistoryService) Stats(ctx context.Context, filter database.ListeningStatFilter) ([]database.ListeningStatRow, error) {
	return s.db.ListeningEventRepository().Stats(ctx, filter)
}

func newPlaySessionID() string {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "play-" + hex.EncodeToString([]byte(time.Now().String()))
	}
	return "play-" + hex.EncodeToString(data)
}
