package mcpserver

import (
	"context"
	"strconv"
	"time"

	"songloft/internal/database"
	"songloft/internal/services"
)

func (s *Server) registerHistoryTools() {
	registerTool(s, ScopeRead, "get_listening_history", "查询全局听歌事件流水，支持 RFC3339 时间范围、区域、歌曲和游标分页；每次播放独立记录，不与续播历史混用。", true,
		func(ctx context.Context, in struct {
			From   time.Time `json:"from,omitempty"`
			To     time.Time `json:"to,omitempty"`
			ZoneID string    `json:"zone_id,omitempty"`
			SongID int64     `json:"song_id,omitempty"`
			Limit  int       `json:"limit,omitempty"`
			Cursor string    `json:"cursor,omitempty"`
		}) (any, error) {
			filter := database.ListeningEventFilter{ZoneID: in.ZoneID, SongID: in.SongID, Limit: pageLimit(in.Limit, 50, 100)}
			if !in.From.IsZero() {
				filter.From = &in.From
			}
			if !in.To.IsZero() {
				filter.To = &in.To
			}
			if filter.From != nil && filter.To != nil && filter.From.After(*filter.To) {
				return nil, invalidInput("from 不能晚于 to")
			}
			if in.Cursor != "" {
				id, err := strconv.ParseInt(in.Cursor, 10, 64)
				if err != nil || id <= 0 {
					return nil, invalidInput("cursor 无效")
				}
				filter.BeforeID = id
			}
			page, err := s.deps.History.List(ctx, filter)
			if err != nil {
				return nil, err
			}
			items := make([]map[string]any, 0, len(page.Items))
			for _, entry := range page.Items {
				items = append(items, map[string]any{
					"event_id": entry.ID, "play_session_id": entry.PlaySessionID,
					"song": songView(entry.Song), "zone_id": entry.ZoneID,
					"context_type": entry.ContextType, "context_key": entry.ContextKey,
					"source": entry.Source, "started_at": entry.StartedAt, "ended_at": entry.EndedAt,
					"played_seconds": entry.PlayedSeconds, "completion_ratio": entry.CompletionRatio,
				})
			}
			return map[string]any{"items": items, "next_cursor": page.NextCursor, "total": len(items)}, nil
		})

	registerTool(s, ScopeRead, "get_listening_stats", "按 song/artist/album/genre/language/style 聚合听歌次数、有效收听次数、时长和最近收听时间。", true,
		func(ctx context.Context, in struct {
			From               time.Time `json:"from,omitempty"`
			To                 time.Time `json:"to,omitempty"`
			GroupBy            string    `json:"group_by"`
			MinCompletionRatio float64   `json:"min_completion_ratio,omitempty"`
			Limit              int       `json:"limit,omitempty"`
		}) (any, error) {
			filter := database.ListeningStatFilter{GroupBy: in.GroupBy, MinCompletionRatio: in.MinCompletionRatio, Limit: pageLimit(in.Limit, 20, 100)}
			if !in.From.IsZero() {
				filter.From = &in.From
			}
			if !in.To.IsZero() {
				filter.To = &in.To
			}
			if filter.MinCompletionRatio < 0 || filter.MinCompletionRatio > 1 {
				return nil, invalidInput("min_completion_ratio 必须在 0-1 之间")
			}
			rows, err := s.deps.History.Stats(ctx, filter)
			if err != nil {
				return nil, err
			}
			return map[string]any{"group_by": in.GroupBy, "items": rows, "total": len(rows)}, nil
		})

	registerTool(s, ScopeRead, "recommend_songs", "根据结构化曲库条件、种子歌曲和听歌历史生成可解释推荐。支持 balanced/similar/familiar/explore；不会自动播放。", true,
		func(ctx context.Context, in struct {
			Filters            searchSongsInput `json:"filters,omitempty"`
			SeedSongIDs        []int64          `json:"seed_song_ids,omitempty"`
			Strategy           string           `json:"strategy,omitempty"`
			Limit              int              `json:"limit,omitempty"`
			ExcludeRecentHours int              `json:"exclude_recent_hours,omitempty"`
			ExcludeSongIDs     []int64          `json:"exclude_song_ids,omitempty"`
		}) (any, error) {
			if in.ExcludeRecentHours < 0 {
				return nil, invalidInput("exclude_recent_hours 不能为负数")
			}
			filter := database.SongFilter{Keyword: in.Filters.Keyword, Type: in.Filters.Type,
				Artist: in.Filters.Artist, Album: in.Filters.Album, Genre: in.Filters.Genre,
				Language: in.Filters.Language, Style: in.Filters.Style, Year: in.Filters.Year,
				DecadeStart: in.Filters.Decade, TagID: in.Filters.TagID, TagName: in.Filters.TagName}
			items, err := s.deps.Recommend.Recommend(ctx, services.RecommendationRequest{
				Filter: filter, SeedSongIDs: in.SeedSongIDs, Strategy: in.Strategy,
				Limit: in.Limit, ExcludeRecentHours: in.ExcludeRecentHours, ExcludeSongIDs: in.ExcludeSongIDs,
			})
			if err != nil {
				return nil, err
			}
			out := make([]map[string]any, 0, len(items))
			for _, item := range items {
				out = append(out, map[string]any{"song": songView(item.Song), "score": item.Score, "reasons": item.Reasons})
			}
			return map[string]any{"recommendations": out, "total": len(out), "autoplay": false}, nil
		})
}

func (s *Server) updateSongTags(ctx context.Context, in struct {
	SongID      int64                  `json:"song_id"`
	Changes     services.MetadataPatch `json:"changes"`
	WriteToFile *bool                  `json:"write_to_file,omitempty"`
}) (any, error) {
	writeToFile := true
	if in.WriteToFile != nil {
		writeToFile = *in.WriteToFile
	}
	result, err := s.deps.Metadata.Update(ctx, services.MetadataUpdateRequest{SongID: in.SongID, Patch: in.Changes, WriteToFile: writeToFile})
	if err != nil {
		return nil, err
	}
	return map[string]any{"success": true, "song_id": result.Song.ID, "song": songView(result.Song),
		"changes": result.Changes, "file_write": result.FileWrite, "warnings": result.Warnings}, nil
}
