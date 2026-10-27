package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"songloft/internal/database"
	"songloft/internal/player"
)

// playbackStore 把 database.PlaybackRepository 适配成 player.Store。
//
// 为什么要适配而不是让 player 直接 import database：
// player 是纯领域包，测试用内存实现就够；把 sqlc 类型泄漏进去会让
// 引擎的单元测试被迫建库。
type playbackStore struct {
	repo *database.PlaybackRepository
}

// newPlaybackStore 创建适配器。
func newPlaybackStore(repo *database.PlaybackRepository) *playbackStore {
	return &playbackStore{repo: repo}
}

func (s *playbackStore) ListPlayers(ctx context.Context) ([]player.Player, error) {
	rows, err := s.repo.ListPlayers(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]player.Player, 0, len(rows))
	for _, r := range rows {
		out = append(out, playerFromRow(r))
	}
	return out, nil
}

func (s *playbackStore) SavePlayer(ctx context.Context, p player.Player) error {
	return s.repo.UpsertPlayer(ctx, playerRow(p))
}

func (s *playbackStore) UpdatePlayerConfig(ctx context.Context, p player.Player) error {
	return s.repo.UpdatePlayerConfig(ctx, p.ID, p.Name, string(p.Takeover), p.VolumeMode, p.MaxVolume)
}

func (s *playbackStore) UpdatePlayerVolume(ctx context.Context, p player.Player) error {
	return s.repo.UpdatePlayerVolume(ctx, p.ID, p.Volume, p.Muted)
}

func (s *playbackStore) UpdatePlayerEQ(ctx context.Context, p player.Player) error {
	return s.repo.UpdatePlayerEQ(ctx, p.ID, player.EQJSON(p.EQ))
}

func (s *playbackStore) DeletePlayer(ctx context.Context, id string) error {
	err := s.repo.DeletePlayer(ctx, id)
	if errors.Is(err, database.ErrNotFound) {
		return player.ErrPlayerNotFound
	}
	return err
}

func (s *playbackStore) ListGroups(ctx context.Context) ([]player.Group, error) {
	rows, err := s.repo.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]player.Group, 0, len(rows))
	for _, r := range rows {
		out = append(out, player.Group{ID: r.ID, Name: r.Name, Icon: r.Icon, MasterID: r.MasterPlayerID, PartyID: r.PartyID})
	}
	return out, nil
}

func (s *playbackStore) SaveGroup(ctx context.Context, g player.Group) error {
	row := database.GroupRow{ID: g.ID, Name: g.Name, Icon: g.Icon, MasterPlayerID: g.MasterID, PartyID: g.PartyID}
	if _, err := s.repo.GetGroup(ctx, g.ID); err != nil {
		if errors.Is(err, database.ErrNotFound) {
			return s.repo.InsertGroup(ctx, row)
		}
		return err
	}
	return s.repo.UpdateGroup(ctx, row)
}

func (s *playbackStore) UpdateGroupIdentity(ctx context.Context, id, name, icon string) error {
	err := s.repo.UpdateGroupIdentity(ctx, id, name, icon)
	if errors.Is(err, database.ErrNotFound) {
		return player.ErrGroupNotFound
	}
	return err
}

func (s *playbackStore) DeleteGroup(ctx context.Context, id string) error {
	err := s.repo.DeleteGroup(ctx, id)
	if errors.Is(err, database.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.repo.DeleteState(ctx, "group", id)
}

func (s *playbackStore) ListAllMembers(ctx context.Context) (map[string][]player.Member, error) {
	rows, err := s.repo.ListAllGroupMembers(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]player.Member, len(rows))
	for _, r := range rows {
		out[r.GroupID] = append(out[r.GroupID], player.Member{PlayerID: r.PlayerID, DelayMs: r.DelayMs, VolumeOffset: r.VolumeOffset, Position: r.Position})
	}
	return out, nil
}

func (s *playbackStore) SaveMember(ctx context.Context, groupID string, m player.Member) error {
	return s.repo.UpsertGroupMember(ctx, database.GroupMemberRow{GroupID: groupID, PlayerID: m.PlayerID, DelayMs: m.DelayMs, VolumeOffset: m.VolumeOffset, Position: m.Position})
}

func (s *playbackStore) UpdateMemberPosition(ctx context.Context, groupID, playerID string, position int) error {
	return s.repo.UpdateGroupMemberPosition(ctx, groupID, playerID, position)
}

func (s *playbackStore) DeleteMember(ctx context.Context, groupID, playerID string) error {
	err := s.repo.RemoveGroupMember(ctx, groupID, playerID)
	if errors.Is(err, database.ErrNotFound) {
		return nil
	}
	return err
}

func (s *playbackStore) SaveQueue(ctx context.Context, ownerKind, ownerID string, songIDs []int64) error {
	return s.repo.ReplaceQueue(ctx, ownerKind, ownerID, songIDs)
}

func (s *playbackStore) LoadQueue(ctx context.Context, ownerKind, ownerID string) ([]int64, error) {
	return s.repo.ListQueue(ctx, ownerKind, ownerID)
}

func (s *playbackStore) SaveState(ctx context.Context, ownerKind, ownerID string, st player.StateSnapshot) error {
	return s.repo.SaveState(ctx, database.PlaybackStateRow{
		OwnerKind:  ownerKind,
		OwnerID:    ownerID,
		SongID:     st.SongID,
		Position:   st.Position,
		State:      st.State,
		RepeatMode: st.RepeatMode,
		Shuffle:    st.Shuffle,
	})
}

func (s *playbackStore) LoadState(ctx context.Context, ownerKind, ownerID string) (player.StateSnapshot, error) {
	row, err := s.repo.GetState(ctx, ownerKind, ownerID)
	if errors.Is(err, database.ErrNotFound) {
		return player.StateSnapshot{}, player.ErrNoState
	}
	if err != nil {
		return player.StateSnapshot{}, err
	}
	return player.StateSnapshot{
		SongID:     row.SongID,
		Position:   row.Position,
		State:      row.State,
		RepeatMode: row.RepeatMode,
		Shuffle:    row.Shuffle,
	}, nil
}

func playerFromRow(r database.PlayerRow) player.Player {
	return player.Player{
		ID:         r.ID,
		Kind:       player.Kind(r.Kind),
		Name:       r.Name,
		Address:    r.Address,
		Model:      r.Model,
		Caps:       player.ParseCaps(r.Capabilities),
		Volume:     r.Volume,
		Muted:      r.Muted,
		VolumeMode: r.VolumeMode,
		MaxVolume:  r.MaxVolume,
		Online:     r.Online,
		Takeover:   player.TakeoverBehavior(r.TakeoverBehavior),
		EQ:         player.ParseEQ(r.EQProfile),
	}
}

func playerRow(p player.Player) database.PlayerRow {
	takeover := string(p.Takeover)
	if takeover == "" {
		takeover = string(player.TakeoverPause)
	}
	volumeMode := p.VolumeMode
	if volumeMode == "" {
		volumeMode = player.VolumeModeDevice
	}
	maxVolume := p.MaxVolume
	if maxVolume <= 0 {
		maxVolume = player.DefaultMaxVolume
	}
	return database.PlayerRow{
		ID:               p.ID,
		Kind:             string(p.Kind),
		Name:             p.Name,
		Address:          p.Address,
		Model:            p.Model,
		Capabilities:     player.CapsJSON(p.Caps),
		Volume:           p.Volume,
		Muted:            p.Muted,
		Online:           p.Online,
		EQProfile:        player.EQJSON(p.EQ),
		TakeoverBehavior: takeover,
		VolumeMode:       volumeMode,
		MaxVolume:        maxVolume,
	}
}

// songTrackInfo 解析曲目时一次性取出的字段。
//
// Format / SampleRate / BitRate 只用于 signal_path 诊断；解码仍由 ffmpeg
// 按实际文件处理，不参与组内格式协商。
type songTrackInfo struct {
	Title      string
	Path       string
	URL        string
	Duration   float64
	Format     string
	SampleRate int
	BitRate    int
	// Artist / Album / ArtworkPath 设备屏显元数据（RAOP DMAP / RAAT
	// update_transport），不参与解码与协商。
	Artist      string
	Album       string
	ArtworkPath string
}

// songTrackResolver 用 SongService 解析。单独定义成函数类型，
// 避免在 app 包里再引一层接口。
type songTrackResolver struct {
	get func(ctx context.Context, id int64) (songTrackInfo, error)
}

func (r songTrackResolver) ResolveTrack(ctx context.Context, songID int64) (player.Track, error) {
	info, err := r.get(ctx, songID)
	if err != nil {
		return player.Track{}, fmt.Errorf("resolve song %d: %w", songID, err)
	}
	t := player.Track{
		SongID:      songID,
		Title:       info.Title,
		Path:        info.Path,
		Duration:    time.Duration(info.Duration * float64(time.Second)),
		Format:      info.Format,
		SampleRate:  info.SampleRate,
		BitRate:     info.BitRate,
		Artist:      info.Artist,
		Album:       info.Album,
		ArtworkPath: info.ArtworkPath,
	}
	if strings.HasPrefix(info.URL, "http://") || strings.HasPrefix(info.URL, "https://") {
		t.RemoteURL = info.URL
	}
	if t.Path == "" && t.RemoteURL == "" {
		return player.Track{}, fmt.Errorf("song %d 没有可播放的本地文件或地址", songID)
	}
	return t, nil
}
