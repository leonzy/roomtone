package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"songloft/internal/database/sqlc"

	sq "github.com/Masterminds/squirrel"
)

type ListeningEventFilter struct {
	From     *time.Time
	To       *time.Time
	ZoneID   string
	SongID   int64
	BeforeID int64
	Limit    int
}

type ListeningStatFilter struct {
	From               *time.Time
	To                 *time.Time
	GroupBy            string
	MinCompletionRatio float64
	Limit              int
}

type ListeningStatRow struct {
	Value          string    `json:"value"`
	PlayCount      int64     `json:"play_count"`
	CompletedCount int64     `json:"completed_count"`
	PlayedSeconds  float64   `json:"played_seconds"`
	LastPlayedAt   time.Time `json:"last_played_at"`
}

type ListeningEventRepository struct {
	db      sqlc.DBTX
	queries *sqlc.Queries
}

func NewListeningEventRepository(db sqlc.DBTX) *ListeningEventRepository {
	return &ListeningEventRepository{db: db, queries: sqlc.New(db)}
}

func (r *ListeningEventRepository) Start(ctx context.Context, arg sqlc.StartListeningEventParams) error {
	return r.queries.StartListeningEvent(ctx, arg)
}

func (r *ListeningEventRepository) GetBySession(ctx context.Context, sessionID string) (sqlc.ListeningEvent, error) {
	event, err := r.queries.GetListeningEventBySession(ctx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return event, ErrNotFound
	}
	return event, err
}

func (r *ListeningEventRepository) FindRecentOpen(ctx context.Context, arg sqlc.FindRecentOpenListeningEventParams) (sqlc.ListeningEvent, error) {
	event, err := r.queries.FindRecentOpenListeningEvent(ctx, arg)
	if errors.Is(err, sql.ErrNoRows) {
		return event, ErrNotFound
	}
	return event, err
}

func (r *ListeningEventRepository) Finish(ctx context.Context, arg sqlc.FinishListeningEventParams) error {
	return r.queries.FinishListeningEvent(ctx, arg)
}

func (r *ListeningEventRepository) RecentSongIDs(ctx context.Context, since time.Time) ([]int64, error) {
	return r.queries.RecentListeningSongIDs(ctx, since)
}

func (r *ListeningEventRepository) FinishLatest(ctx context.Context, songID int64, source string, endedAt time.Time, playedSeconds, completionRatio float64) error {
	id, err := r.queries.LatestOpenListeningEventID(ctx, sqlc.LatestOpenListeningEventIDParams{SongID: songID, Source: source})
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return r.queries.FinishListeningEventByID(ctx, sqlc.FinishListeningEventByIDParams{
		ID: id, EndedAt: sql.NullTime{Time: endedAt, Valid: true},
		PlayedSeconds: playedSeconds, CompletionRatio: completionRatio,
	})
}

func (r *ListeningEventRepository) List(ctx context.Context, filter ListeningEventFilter) ([]sqlc.ListeningEvent, error) {
	query := sq.Select("id", "play_session_id", "song_id", "zone_id", "context_type", "context_key", "client_id", "source", "started_at", "ended_at", "played_seconds", "completion_ratio", "created_at", "updated_at").
		From("listening_events")
	if filter.From != nil {
		query = query.Where(sq.GtOrEq{"started_at": *filter.From})
	}
	if filter.To != nil {
		query = query.Where(sq.LtOrEq{"started_at": *filter.To})
	}
	if filter.ZoneID != "" {
		query = query.Where(sq.Eq{"zone_id": filter.ZoneID})
	}
	if filter.SongID > 0 {
		query = query.Where(sq.Eq{"song_id": filter.SongID})
	}
	if filter.BeforeID > 0 {
		query = query.Where(sq.Lt{"id": filter.BeforeID})
	}
	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query = query.OrderBy("started_at DESC", "id DESC").Limit(uint64(limit))
	sqlText, args, err := query.ToSql()
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("list listening events: %w", err)
	}
	defer rows.Close()
	out := []sqlc.ListeningEvent{}
	for rows.Next() {
		var event sqlc.ListeningEvent
		if err := rows.Scan(&event.ID, &event.PlaySessionID, &event.SongID, &event.ZoneID,
			&event.ContextType, &event.ContextKey, &event.ClientID, &event.Source,
			&event.StartedAt, &event.EndedAt, &event.PlayedSeconds, &event.CompletionRatio,
			&event.CreatedAt, &event.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, event)
	}
	return out, rows.Err()
}

func (r *ListeningEventRepository) Stats(ctx context.Context, filter ListeningStatFilter) ([]ListeningStatRow, error) {
	columns := map[string]string{"song": "CAST(song_id AS TEXT)", "artist": "songs.artist", "album": "songs.album", "genre": "songs.genre", "language": "songs.language", "style": "songs.style"}
	column, ok := columns[filter.GroupBy]
	if !ok {
		return nil, fmt.Errorf("invalid listening stats group %q", filter.GroupBy)
	}
	completion := strconv.FormatFloat(filter.MinCompletionRatio, 'f', -1, 64)
	query := sq.Select(column+" AS value", "COUNT(*) AS play_count",
		"SUM(CASE WHEN completion_ratio >= "+completion+" THEN 1 ELSE 0 END) AS completed_count",
		"SUM(played_seconds) AS played_seconds", "MAX(started_at) AS last_played_at").
		From("listening_events").Join("songs ON songs.id = listening_events.song_id")
	if filter.From != nil {
		query = query.Where(sq.GtOrEq{"started_at": *filter.From})
	}
	if filter.To != nil {
		query = query.Where(sq.LtOrEq{"started_at": *filter.To})
	}
	limit := filter.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	query = query.GroupBy(column).OrderBy("play_count DESC", "value ASC").Limit(uint64(limit))
	sqlText, args, err := query.ToSql()
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("listening stats: %w", err)
	}
	defer rows.Close()
	out := []ListeningStatRow{}
	for rows.Next() {
		var row ListeningStatRow
		var lastPlayed string
		if err := rows.Scan(&row.Value, &row.PlayCount, &row.CompletedCount, &row.PlayedSeconds, &lastPlayed); err != nil {
			return nil, err
		}
		row.LastPlayedAt, _ = parseAlarmTime(lastPlayed)
		out = append(out, row)
	}
	return out, rows.Err()
}
