package mcpserver

import (
	"context"

	"songloft/internal/database"
	"songloft/internal/models"
)

type searchSongsInput struct {
	Keyword  string `json:"keyword,omitempty"`
	Type     string `json:"type,omitempty"`
	Artist   string `json:"artist,omitempty"`
	Album    string `json:"album,omitempty"`
	Genre    string `json:"genre,omitempty"`
	Language string `json:"language,omitempty"`
	Style    string `json:"style,omitempty"`
	Year     int    `json:"year,omitempty"`
	Decade   int    `json:"decade,omitempty"`
	TagID    int64  `json:"tag_id,omitempty"`
	TagName  string `json:"tag,omitempty"`
	Limit    int    `json:"limit,omitempty"`
	Offset   int    `json:"offset,omitempty"`
}

func (s *Server) registerLibraryTools() {
	registerTool(s, ScopeRead, "search_songs", "搜索 Roomtone 曲库。支持关键词及歌手、专辑、流派、语种、风格、年份、年代和自定义标签过滤；返回稳定 song_id 供后续播放或编辑。", true,
		func(ctx context.Context, in searchSongsInput) (any, error) {
			if in.Offset < 0 {
				return nil, invalidInput("offset 不能为负数")
			}
			filter := &database.SongFilter{
				Keyword: in.Keyword, Type: in.Type, Artist: in.Artist, Album: in.Album,
				Genre: in.Genre, Language: in.Language, Style: in.Style, Year: in.Year,
				DecadeStart: in.Decade, TagID: in.TagID, TagName: in.TagName,
				ExcludePlaylistLabels: []string{models.PlaylistLabelHidden},
				Limit:                 pageLimit(in.Limit, 20, 50), Offset: in.Offset,
			}
			songs, err := s.deps.Songs.List(ctx, filter)
			if err != nil {
				return nil, err
			}
			total, err := s.deps.Songs.Count(ctx, filter)
			if err != nil {
				return nil, err
			}
			return map[string]any{"songs": songViews(songs), "total": total, "limit": filter.Limit, "offset": filter.Offset}, nil
		})

	registerTool(s, ScopeRead, "get_songs", "按 song_id 批量获取歌曲，最多 100 首；不存在的 ID 会在 missing_ids 中返回。", true,
		func(ctx context.Context, in struct {
			SongIDs []int64 `json:"song_ids"`
		}) (any, error) {
			if len(in.SongIDs) == 0 || len(in.SongIDs) > 100 {
				return nil, invalidInput("song_ids 数量必须为 1-100")
			}
			songs := make([]SongView, 0, len(in.SongIDs))
			missing := []int64{}
			for _, id := range in.SongIDs {
				if id <= 0 {
					return nil, invalidInput("song_id 必须为正整数")
				}
				song, err := s.deps.Songs.GetByID(ctx, id)
				if err != nil {
					missing = append(missing, id)
					continue
				}
				songs = append(songs, songView(song))
			}
			return map[string]any{"songs": songs, "missing_ids": missing, "total": len(songs)}, nil
		})

	registerTool(s, ScopeRead, "list_playlists", "列出 Roomtone 歌单，支持关键词、类型和分页。", true,
		func(ctx context.Context, in struct {
			Keyword string `json:"keyword,omitempty"`
			Type    string `json:"type,omitempty"`
			Limit   int    `json:"limit,omitempty"`
			Offset  int    `json:"offset,omitempty"`
		}) (any, error) {
			if in.Offset < 0 {
				return nil, invalidInput("offset 不能为负数")
			}
			filter := &database.PlaylistFilter{Keyword: in.Keyword, Type: in.Type, Limit: pageLimit(in.Limit, 20, 50), Offset: in.Offset}
			playlists, err := s.deps.Playlists.List(ctx, filter)
			if err != nil {
				return nil, err
			}
			total, err := s.deps.Playlists.Count(ctx, &database.PlaylistFilter{Keyword: in.Keyword, Type: in.Type})
			if err != nil {
				return nil, err
			}
			return map[string]any{"playlists": playlists, "total": total, "limit": filter.Limit, "offset": filter.Offset}, nil
		})

	registerTool(s, ScopeRead, "get_playlist_songs", "按 playlist_id 获取歌单歌曲，返回的 song_id 可直接用于播放。", true,
		func(ctx context.Context, in struct {
			PlaylistID int64 `json:"playlist_id"`
			Limit      int   `json:"limit,omitempty"`
			Offset     int   `json:"offset,omitempty"`
		}) (any, error) {
			if in.PlaylistID <= 0 || in.Offset < 0 {
				return nil, invalidInput("playlist_id 必须为正整数，offset 不能为负数")
			}
			if _, err := s.deps.Playlists.GetByID(ctx, in.PlaylistID); err != nil {
				return nil, err
			}
			filter := database.PlaylistSongFilter{Limit: pageLimit(in.Limit, 50, 100), Offset: in.Offset}
			songs, err := s.deps.Playlists.GetSongs(ctx, in.PlaylistID, filter)
			if err != nil {
				return nil, err
			}
			return map[string]any{"playlist_id": in.PlaylistID, "songs": songViews(songs), "total": len(songs), "limit": filter.Limit, "offset": filter.Offset}, nil
		})

	registerTool(s, ScopeMetadata, "update_song_tags", "修改明确 song_id 的标题、歌手、专辑、年份、流派、语种、风格或轨号；本地歌曲默认同时回写音频标签，不重命名或移动文件。", false, s.updateSongTags)
}
