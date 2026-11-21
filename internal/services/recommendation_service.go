package services

import (
	"context"
	"math"
	"sort"
	"time"

	"songloft/internal/database"
	"songloft/internal/models"
)

type RecommendationRequest struct {
	Filter             database.SongFilter
	SeedSongIDs        []int64
	Strategy           string
	Limit              int
	ExcludeRecentHours int
	ExcludeSongIDs     []int64
}

type RecommendationReason struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Recommendation struct {
	Song    *models.Song           `json:"song"`
	Score   float64                `json:"score"`
	Reasons []RecommendationReason `json:"reasons"`
}

type RecommendationService struct {
	db  database.DB
	now func() time.Time
}

func NewRecommendationService(db database.DB) *RecommendationService {
	return &RecommendationService{db: db, now: time.Now}
}

func (s *RecommendationService) Recommend(ctx context.Context, req RecommendationRequest) ([]Recommendation, error) {
	if req.Limit <= 0 || req.Limit > 50 {
		req.Limit = 10
	}
	switch req.Strategy {
	case "", "balanced", "similar", "familiar", "explore":
	default:
		return nil, models.ErrInvalidType
	}
	filter := req.Filter
	filter.Limit = 500
	filter.Offset = 0
	filter.ExcludePlaylistLabels = []string{models.PlaylistLabelHidden}
	candidates, err := s.db.SongRepository().List(ctx, &filter)
	if err != nil {
		return nil, err
	}
	excluded := make(map[int64]bool, len(req.ExcludeSongIDs))
	for _, id := range req.ExcludeSongIDs {
		excluded[id] = true
	}
	if req.ExcludeRecentHours > 0 {
		ids, err := s.db.ListeningEventRepository().RecentSongIDs(ctx, s.now().Add(-time.Duration(req.ExcludeRecentHours)*time.Hour))
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			excluded[id] = true
		}
	}
	seeds := make([]*models.Song, 0, len(req.SeedSongIDs))
	for _, id := range req.SeedSongIDs {
		song, err := s.db.SongRepository().GetByID(ctx, id)
		if err == nil {
			seeds = append(seeds, song)
		}
	}
	artistAffinity, genreAffinity, historyAvailable, err := s.affinities(ctx)
	if err != nil {
		return nil, err
	}
	scored := make([]Recommendation, 0, len(candidates))
	for _, song := range candidates {
		if song == nil || excluded[song.ID] || song.Type == models.TypeRadio || song.IsVideo {
			continue
		}
		recommendation := scoreSong(song, seeds, artistAffinity, genreAffinity, req.Strategy)
		if !historyAvailable && len(seeds) == 0 {
			recommendation.Reasons = append(recommendation.Reasons, RecommendationReason{Code: "explore_cold_start", Message: "暂无足够听歌偏好，按曲库多样性探索"})
		}
		scored = append(scored, recommendation)
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].Score == scored[j].Score {
			return scored[i].Song.ID < scored[j].Song.ID
		}
		return scored[i].Score > scored[j].Score
	})
	out := make([]Recommendation, 0, req.Limit)
	artistCounts := map[string]int{}
	for _, item := range scored {
		if artistCounts[item.Song.Artist] >= 2 && len(scored)-len(out) > req.Limit {
			continue
		}
		artistCounts[item.Song.Artist]++
		out = append(out, item)
		if len(out) == req.Limit {
			break
		}
	}
	if len(out) < req.Limit {
		for _, item := range scored {
			found := false
			for _, selected := range out {
				if selected.Song.ID == item.Song.ID {
					found = true
					break
				}
			}
			if !found {
				out = append(out, item)
			}
			if len(out) == req.Limit || len(out) == len(scored) {
				break
			}
		}
	}
	return out, nil
}

func (s *RecommendationService) affinities(ctx context.Context) (map[string]float64, map[string]float64, bool, error) {
	artists, err := s.db.ListeningEventRepository().Stats(ctx, database.ListeningStatFilter{GroupBy: "artist", MinCompletionRatio: 0.8, Limit: 20})
	if err != nil {
		return nil, nil, false, err
	}
	genres, err := s.db.ListeningEventRepository().Stats(ctx, database.ListeningStatFilter{GroupBy: "genre", MinCompletionRatio: 0.8, Limit: 20})
	if err != nil {
		return nil, nil, false, err
	}
	artistMap := make(map[string]float64, len(artists))
	for _, row := range artists {
		artistMap[row.Value] = math.Log1p(float64(row.CompletedCount))
	}
	genreMap := make(map[string]float64, len(genres))
	for _, row := range genres {
		genreMap[row.Value] = math.Log1p(float64(row.CompletedCount))
	}
	return artistMap, genreMap, len(artists)+len(genres) > 0, nil
}

func scoreSong(song *models.Song, seeds []*models.Song, artists, genres map[string]float64, strategy string) Recommendation {
	raw := 1.0
	reasons := []RecommendationReason{}
	for _, seed := range seeds {
		if seed.ID == song.ID {
			raw -= 2
			continue
		}
		if seed.Artist != "" && seed.Artist == song.Artist {
			raw += 1.2
			reasons = appendReason(reasons, "seed_artist", "与种子歌曲歌手相同")
		}
		if seed.Genre != "" && seed.Genre == song.Genre {
			raw += 0.8
			reasons = appendReason(reasons, "seed_genre", "与种子歌曲流派相近")
		}
		if seed.Style != "" && seed.Style == song.Style {
			raw += 0.8
			reasons = appendReason(reasons, "seed_style", "与种子歌曲风格相同")
		}
		if seed.Language != "" && seed.Language == song.Language {
			raw += 0.3
		}
	}
	artistScore := artists[song.Artist]
	genreScore := genres[song.Genre]
	if artistScore > 0 {
		reasons = appendReason(reasons, "artist_affinity", "符合历史歌手偏好")
	}
	if genreScore > 0 {
		reasons = appendReason(reasons, "genre_affinity", "符合历史流派偏好")
	}
	switch strategy {
	case "similar":
		raw += artistScore*0.1 + genreScore*0.1
	case "familiar":
		raw += artistScore*0.8 + genreScore*0.5
	case "explore":
		raw -= artistScore*0.5 + genreScore*0.3
	default:
		raw += artistScore*0.4 + genreScore*0.25
	}
	if raw < 0 {
		raw = 0
	}
	return Recommendation{Song: song, Score: 1 - math.Exp(-raw/4), Reasons: reasons}
}

func appendReason(reasons []RecommendationReason, code, message string) []RecommendationReason {
	for _, reason := range reasons {
		if reason.Code == code {
			return reasons
		}
	}
	return append(reasons, RecommendationReason{Code: code, Message: message})
}
