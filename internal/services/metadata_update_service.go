package services

import (
	"context"
	"errors"
	"strings"

	"songloft/internal/database"
	"songloft/internal/models"
)

type MetadataPatch struct {
	Title    *string              `json:"title,omitempty"`
	Artist   *string              `json:"artist,omitempty"`
	Artists  []models.ArtistInput `json:"artists,omitempty"`
	Album    *string              `json:"album,omitempty"`
	Year     *int                 `json:"year,omitempty"`
	Genre    *string              `json:"genre,omitempty"`
	Language *string              `json:"language,omitempty"`
	Style    *string              `json:"style,omitempty"`
	Track    *string              `json:"track,omitempty"`
}

type MetadataUpdateRequest struct {
	SongID      int64
	Patch       MetadataPatch
	WriteToFile bool
}

type MetadataUpdateResult struct {
	Song      *models.Song    `json:"song"`
	Changes   map[string]any  `json:"changes"`
	FileWrite FileWriteStatus `json:"file_write"`
	Warnings  []string        `json:"warnings,omitempty"`
}

type MetadataUpdateService struct {
	db database.DB
}

func NewMetadataUpdateService(db database.DB) *MetadataUpdateService {
	return &MetadataUpdateService{db: db}
}

func (s *MetadataUpdateService) Update(ctx context.Context, req MetadataUpdateRequest) (MetadataUpdateResult, error) {
	if req.SongID <= 0 {
		return MetadataUpdateResult{}, models.ErrMissingSong
	}
	if req.Patch.Title == nil && req.Patch.Artist == nil && req.Patch.Artists == nil &&
		req.Patch.Album == nil && req.Patch.Year == nil && req.Patch.Genre == nil &&
		req.Patch.Language == nil && req.Patch.Style == nil && req.Patch.Track == nil {
		return MetadataUpdateResult{}, errors.New("changes 至少需要一个字段")
	}
	if req.Patch.Artist != nil && req.Patch.Artists != nil {
		return MetadataUpdateResult{}, errors.New("artist 与 artists 不能同时设置")
	}
	existing, err := s.db.SongRepository().GetByID(ctx, req.SongID)
	if err != nil {
		return MetadataUpdateResult{}, err
	}
	if req.WriteToFile && existing.CueSourcePath != "" {
		return MetadataUpdateResult{}, errors.New("CUE 拆分歌曲不能单独回写共享整轨文件；请使用 write_to_file=false")
	}
	result := MetadataUpdateResult{Changes: map[string]any{}, FileWrite: FileWriteUnchanged}
	var song *models.Song
	err = s.db.RunInTx(ctx, func(ctx context.Context, uow *database.UnitOfWork) error {
		var err error
		song, err = uow.Songs.GetByID(ctx, req.SongID)
		if err != nil {
			return err
		}
		before := *song
		applyMetadataPatch(song, req.Patch)
		if req.Patch.Title != nil && strings.TrimSpace(song.Title) == "" {
			return errors.New("标题不能为空")
		}
		if req.Patch.Year != nil && song.Year < 0 {
			return errors.New("year 不能为负数")
		}
		if req.Patch.Artist != nil || req.Patch.Artists != nil {
			inputs := req.Patch.Artists
			if req.Patch.Artist != nil {
				current, err := uow.SongArtists.GetBySongID(ctx, req.SongID)
				if err != nil {
					return err
				}
				inputs = []models.ArtistInput{}
				for _, artist := range current {
					if artist.Role == models.ArtistRoleAlbumArtist {
						inputs = append(inputs, models.ArtistInput{Name: artist.Artist.Name, Role: artist.Role, Position: artist.Position})
					}
				}
				inputs = append(inputs, models.ArtistInput{Name: strings.TrimSpace(*req.Patch.Artist), Role: models.ArtistRoleArtist})
				if strings.TrimSpace(*req.Patch.Artist) == "" {
					inputs = inputs[:len(inputs)-1]
				}
			}
			if err := uow.SongArtists.SetSongArtists(ctx, req.SongID, inputs); err != nil {
				return err
			}
			song.Artist = displayArtistFromInputs(inputs)
		}
		if err := song.Validate(); err != nil {
			return err
		}
		if err := uow.Songs.Update(ctx, song); err != nil {
			return err
		}
		result.Changes = metadataChanges(before, *song)
		return nil
	})
	if err != nil {
		return MetadataUpdateResult{}, err
	}
	result.Song = song
	if !req.WriteToFile {
		return result, nil
	}
	if song.Type != models.TypeLocal || song.FilePath == "" {
		result.Warnings = append(result.Warnings, "非本地歌曲没有可回写的源音频文件")
		return result, nil
	}
	result.FileWrite = WriteSongTags(song.FilePath, song)
	if result.FileWrite == FileWriteFailed {
		result.Warnings = append(result.Warnings, "数据库已更新，但音频文件标签写入失败")
	}
	return result, nil
}

func applyMetadataPatch(song *models.Song, patch MetadataPatch) {
	if patch.Title != nil {
		song.Title = strings.TrimSpace(*patch.Title)
	}
	if patch.Album != nil {
		song.Album = strings.TrimSpace(*patch.Album)
	}
	if patch.Year != nil {
		song.Year = *patch.Year
	}
	if patch.Genre != nil {
		song.Genre = strings.TrimSpace(*patch.Genre)
	}
	if patch.Language != nil {
		song.Language = strings.TrimSpace(*patch.Language)
	}
	if patch.Style != nil {
		song.Style = strings.TrimSpace(*patch.Style)
	}
	if patch.Track != nil {
		song.Track = strings.TrimSpace(*patch.Track)
	}
}

func metadataChanges(before, after models.Song) map[string]any {
	changes := map[string]any{}
	add := func(field string, old, new any) {
		if old != new {
			changes[field] = map[string]any{"from": old, "to": new}
		}
	}
	add("title", before.Title, after.Title)
	add("artist", before.Artist, after.Artist)
	add("album", before.Album, after.Album)
	add("year", before.Year, after.Year)
	add("genre", before.Genre, after.Genre)
	add("language", before.Language, after.Language)
	add("style", before.Style, after.Style)
	add("track", before.Track, after.Track)
	return changes
}
