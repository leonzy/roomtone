package mcpserver

import (
	"time"

	"songloft/internal/models"
	"songloft/internal/player"
)

type SongView struct {
	ID        int64     `json:"id"`
	Type      string    `json:"type"`
	Title     string    `json:"title"`
	Artist    string    `json:"artist"`
	Album     string    `json:"album"`
	Year      int       `json:"year"`
	Genre     string    `json:"genre"`
	Language  string    `json:"language,omitempty"`
	Style     string    `json:"style,omitempty"`
	Duration  float64   `json:"duration"`
	CoverURL  string    `json:"cover_url,omitempty"`
	Format    string    `json:"format,omitempty"`
	IsLive    bool      `json:"is_live"`
	IsVideo   bool      `json:"is_video"`
	AddedAt   time.Time `json:"added_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func songView(s *models.Song) SongView {
	if s == nil {
		return SongView{}
	}
	return SongView{
		ID: s.ID, Type: s.Type, Title: s.Title, Artist: s.Artist, Album: s.Album,
		Year: s.Year, Genre: s.Genre, Language: s.Language, Style: s.Style,
		Duration: s.Duration, CoverURL: s.CoverURLPath(), Format: s.Format,
		IsLive: s.IsLive, IsVideo: s.IsVideo, AddedAt: s.AddedAt, UpdatedAt: s.UpdatedAt,
	}
}

func songViews(songs []*models.Song) []SongView {
	out := make([]SongView, 0, len(songs))
	for _, song := range songs {
		out = append(out, songView(song))
	}
	return out
}

type SpeakerCapsView struct {
	Codecs         []player.Codec `json:"codecs"`
	MaxSampleRate  int            `json:"max_sample_rate"`
	Channels       int            `json:"channels"`
	BitDepth       int            `json:"bit_depth"`
	SupportsVolume bool           `json:"supports_volume"`
	SupportsSeek   bool           `json:"supports_seek"`
	SyncTier       string         `json:"sync_tier"`
	AirPlay2       bool           `json:"airplay2"`
}

type SpeakerView struct {
	ID              string                  `json:"id"`
	Kind            player.Kind             `json:"kind"`
	Name            string                  `json:"name"`
	Model           string                  `json:"model,omitempty"`
	Online          bool                    `json:"online"`
	Standby         bool                    `json:"standby"`
	ZoneID          string                  `json:"zone_id"`
	Volume          int                     `json:"volume"`
	Muted           bool                    `json:"muted"`
	VolumeMode      string                  `json:"volume_mode"`
	MaxVolume       int                     `json:"max_volume"`
	DLNASyncDelayMs int                     `json:"dlna_sync_delay_ms,omitempty"`
	Takeover        player.TakeoverBehavior `json:"takeover_behavior,omitempty"`
	EQ              player.EQProfile        `json:"eq"`
	Caps            SpeakerCapsView         `json:"capabilities"`
}

func speakerView(p player.Player, zoneID string) SpeakerView {
	return SpeakerView{
		ID: p.ID, Kind: p.Kind, Name: p.Name, Model: p.Model, Online: p.Online,
		Standby: p.Standby, ZoneID: zoneID, Volume: p.Volume, Muted: p.Muted,
		VolumeMode: p.VolumeMode, MaxVolume: p.MaxVolume, DLNASyncDelayMs: p.DLNASyncDelayMs,
		Takeover: p.Takeover, EQ: p.EQ,
		Caps: SpeakerCapsView{
			Codecs: p.Caps.Codecs, MaxSampleRate: p.Caps.MaxSampleRate, Channels: p.Caps.Channels,
			BitDepth: p.Caps.BitDepth, SupportsVolume: p.Caps.SupportsVolume,
			SupportsSeek: p.Caps.SupportsSeek, SyncTier: p.Caps.SyncTier.String(), AirPlay2: p.Caps.AirPlay2,
		},
	}
}

type ZoneView struct {
	player.Group
	SyncTier   string              `json:"sync_tier"`
	MemberCaps []player.MemberCaps `json:"member_caps"`
	Status     player.Status       `json:"status"`
}

func zoneView(g player.GroupView) ZoneView {
	return ZoneView{Group: g.Group, SyncTier: g.SyncTier, MemberCaps: g.MemberCaps, Status: g.Status}
}

type MutationResult struct {
	Success         bool           `json:"success"`
	Object          any            `json:"object,omitempty"`
	Changes         map[string]any `json:"changes,omitempty"`
	Warnings        []string       `json:"warnings,omitempty"`
	PartialFailures []string       `json:"partial_failures,omitempty"`
}

func mutation(object any, changes map[string]any) MutationResult {
	return MutationResult{Success: true, Object: object, Changes: changes}
}

func pageLimit(value, defaultValue, maximum int) int {
	if value <= 0 {
		return defaultValue
	}
	if value > maximum {
		return maximum
	}
	return value
}
