package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"songloft/internal/database/sqlc"
)

// PlaybackRepository 播放器 / 同步组 / 派对 / 队列 / 状态 的仓储。
//
// 全部是固定 SQL（queries/playback.sql + sqlc），没有变长 WHERE，
// 因此不引入 squirrel。队列重排按"整表重写"实现，见 ReplaceQueue。
type PlaybackRepository struct {
	db      sqlc.DBTX
	queries *sqlc.Queries
}

// NewPlaybackRepository 创建仓储实例
func NewPlaybackRepository(db sqlc.DBTX) *PlaybackRepository {
	return &PlaybackRepository{db: db, queries: sqlc.New(db)}
}

// PlayerRow 播放设备行（capabilities / eq_profile 是 JSON 文本，由调用方编解码）
type PlayerRow struct {
	ID               string
	Kind             string
	Name             string
	Address          string
	Model            string
	Capabilities     string
	Volume           int
	Muted            bool
	Online           bool
	EQProfile        string
	TakeoverBehavior string
	VolumeMode       string
	MaxVolume        int
	LastSeenAt       time.Time
	CreatedAt        time.Time
}

// GroupRow 同步组行
type GroupRow struct {
	ID             string
	Name           string
	Icon           string
	MasterPlayerID string
	PartyID        string
	CreatedAt      time.Time
}

// GroupMemberRow 组成员行
type GroupMemberRow struct {
	GroupID      string
	PlayerID     string
	DelayMs      int
	VolumeOffset int
	// Position 组内显示顺序（决定"主控候选"顺序）。0 表示未显式指定。
	Position int
}

// PlaybackStateRow 播放状态行（断点续播的落库形态）
type PlaybackStateRow struct {
	OwnerKind  string
	OwnerID    string
	SongID     int64
	Position   float64
	State      string
	RepeatMode string
	Shuffle    bool
	UpdatedAt  time.Time
}

func playerRowFromSQLc(r sqlc.Player) PlayerRow {
	out := PlayerRow{
		ID:               r.ID,
		Kind:             r.Kind,
		Name:             r.Name,
		Address:          r.Address,
		Model:            r.Model,
		Capabilities:     r.Capabilities,
		Volume:           int(r.Volume),
		Muted:            r.Muted != 0,
		Online:           r.Online != 0,
		EQProfile:        r.EqProfile,
		TakeoverBehavior: r.TakeoverBehavior,
		VolumeMode:       r.VolumeMode,
		MaxVolume:        int(r.MaxVolume),
		CreatedAt:        r.CreatedAt,
	}
	if r.LastSeenAt.Valid {
		out.LastSeenAt = r.LastSeenAt.Time
	}
	return out
}

// ListPlayers 列出全部设备
func (r *PlaybackRepository) ListPlayers(ctx context.Context) ([]PlayerRow, error) {
	rows, err := r.queries.ListPlayers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list players: %w", err)
	}
	out := make([]PlayerRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, playerRowFromSQLc(row))
	}
	return out, nil
}

// GetPlayer 取单个设备，未命中返回 ErrNotFound
func (r *PlaybackRepository) GetPlayer(ctx context.Context, id string) (PlayerRow, error) {
	row, err := r.queries.GetPlayer(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return PlayerRow{}, ErrNotFound
	}
	if err != nil {
		return PlayerRow{}, fmt.Errorf("get player: %w", err)
	}
	return playerRowFromSQLc(row), nil
}

// UpsertPlayer 注册或刷新设备（保留已存的 volume / eq_profile / takeover_behavior）
func (r *PlaybackRepository) UpsertPlayer(ctx context.Context, p PlayerRow) error {
	var lastSeen sql.NullTime
	if !p.LastSeenAt.IsZero() {
		lastSeen = sql.NullTime{Time: p.LastSeenAt, Valid: true}
	}
	err := r.queries.UpsertPlayer(ctx, sqlc.UpsertPlayerParams{
		ID:               p.ID,
		Kind:             p.Kind,
		Name:             p.Name,
		Address:          p.Address,
		Model:            p.Model,
		Capabilities:     p.Capabilities,
		Volume:           int64(p.Volume),
		Muted:            boolToInt64(p.Muted),
		Online:           boolToInt64(p.Online),
		EqProfile:        p.EQProfile,
		TakeoverBehavior: p.TakeoverBehavior,
		VolumeMode:       p.VolumeMode,
		MaxVolume:        int64(p.MaxVolume),
		LastSeenAt:       lastSeen,
	})
	if err != nil {
		return fmt.Errorf("upsert player: %w", err)
	}
	return nil
}

// UpdatePlayerConfig 更新名称、接管行为、音量模式与音量上限
func (r *PlaybackRepository) UpdatePlayerConfig(ctx context.Context, id, name, takeover, volumeMode string, maxVolume int) error {
	return r.queries.UpdatePlayerConfig(ctx, sqlc.UpdatePlayerConfigParams{
		Name:             name,
		TakeoverBehavior: takeover,
		VolumeMode:       volumeMode,
		MaxVolume:        int64(maxVolume),
		ID:               id,
	})
}

// UpdatePlayerVolume 更新音量与静音
func (r *PlaybackRepository) UpdatePlayerVolume(ctx context.Context, id string, volume int, muted bool) error {
	return r.queries.UpdatePlayerVolume(ctx, sqlc.UpdatePlayerVolumeParams{Volume: int64(volume), Muted: boolToInt64(muted), ID: id})
}

// UpdatePlayerEQ 更新 EQ 配置 JSON
func (r *PlaybackRepository) UpdatePlayerEQ(ctx context.Context, id, eqJSON string) error {
	return r.queries.UpdatePlayerEQ(ctx, sqlc.UpdatePlayerEQParams{EqProfile: eqJSON, ID: id})
}

// UpdatePlayerOnline 更新在线状态
func (r *PlaybackRepository) UpdatePlayerOnline(ctx context.Context, id string, online bool) error {
	now := sql.NullTime{Time: time.Now(), Valid: true}
	return r.queries.UpdatePlayerOnline(ctx, sqlc.UpdatePlayerOnlineParams{Online: boolToInt64(online), LastSeenAt: now, ID: id})
}

// DeletePlayer 删除设备，未命中返回 ErrNotFound
func (r *PlaybackRepository) DeletePlayer(ctx context.Context, id string) error {
	n, err := r.queries.DeletePlayer(ctx, id)
	if err != nil {
		return fmt.Errorf("delete player: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListGroups 列出全部同步组
func (r *PlaybackRepository) ListGroups(ctx context.Context) ([]GroupRow, error) {
	rows, err := r.queries.ListGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	out := make([]GroupRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, GroupRow{
			ID:             row.ID,
			Name:           row.Name,
			Icon:           row.Icon,
			MasterPlayerID: row.MasterPlayerID,
			PartyID:        row.PartyID.String,
			CreatedAt:      row.CreatedAt,
		})
	}
	return out, nil
}

// GetGroup 取单个组，未命中返回 ErrNotFound
func (r *PlaybackRepository) GetGroup(ctx context.Context, id string) (GroupRow, error) {
	row, err := r.queries.GetGroup(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return GroupRow{}, ErrNotFound
	}
	if err != nil {
		return GroupRow{}, fmt.Errorf("get group: %w", err)
	}
	return GroupRow{ID: row.ID, Name: row.Name, Icon: row.Icon, MasterPlayerID: row.MasterPlayerID, PartyID: row.PartyID.String, CreatedAt: row.CreatedAt}, nil
}

// InsertGroup 新建组
func (r *PlaybackRepository) InsertGroup(ctx context.Context, g GroupRow) error {
	var party sql.NullString
	if g.PartyID != "" {
		party = sql.NullString{String: g.PartyID, Valid: true}
	}
	if err := r.queries.InsertGroup(ctx, sqlc.InsertGroupParams{ID: g.ID, Name: g.Name, MasterPlayerID: g.MasterPlayerID, PartyID: party}); err != nil {
		return fmt.Errorf("insert group: %w", err)
	}
	return nil
}

// UpdateGroup 更新组名称 / 主控 / 派对归属
func (r *PlaybackRepository) UpdateGroup(ctx context.Context, g GroupRow) error {
	var party sql.NullString
	if g.PartyID != "" {
		party = sql.NullString{String: g.PartyID, Valid: true}
	}
	return r.queries.UpdateGroup(ctx, sqlc.UpdateGroupParams{Name: g.Name, MasterPlayerID: g.MasterPlayerID, PartyID: party, ID: g.ID})
}

// UpdateGroupIdentity 更新组名称与图标，未命中返回 ErrNotFound
func (r *PlaybackRepository) UpdateGroupIdentity(ctx context.Context, id, name, icon string) error {
	n, err := r.queries.UpdateGroupIdentity(ctx, sqlc.UpdateGroupIdentityParams{Name: name, Icon: icon, ID: id})
	if err != nil {
		return fmt.Errorf("update group identity: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteGroup 删除组，未命中返回 ErrNotFound
func (r *PlaybackRepository) DeleteGroup(ctx context.Context, id string) error {
	n, err := r.queries.DeleteGroup(ctx, id)
	if err != nil {
		return fmt.Errorf("delete group: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListAllGroupMembers 列出全部组成员（启动恢复用）
func (r *PlaybackRepository) ListAllGroupMembers(ctx context.Context) ([]GroupMemberRow, error) {
	rows, err := r.queries.ListAllGroupMembers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list all group members: %w", err)
	}
	out := make([]GroupMemberRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, GroupMemberRow{GroupID: row.GroupID, PlayerID: row.PlayerID, DelayMs: int(row.DelayMs), VolumeOffset: int(row.VolumeOffset), Position: int(row.Position)})
	}
	return out, nil
}

// ListGroupMembers 列出单个组成员
func (r *PlaybackRepository) ListGroupMembers(ctx context.Context, groupID string) ([]GroupMemberRow, error) {
	rows, err := r.queries.ListGroupMembers(ctx, groupID)
	if err != nil {
		return nil, fmt.Errorf("list group members: %w", err)
	}
	out := make([]GroupMemberRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, GroupMemberRow{GroupID: row.GroupID, PlayerID: row.PlayerID, DelayMs: int(row.DelayMs), VolumeOffset: int(row.VolumeOffset), Position: int(row.Position)})
	}
	return out, nil
}

// UpsertGroupMember 加入成员（重复加入按最新 delay/volume_offset 覆盖）
func (r *PlaybackRepository) UpsertGroupMember(ctx context.Context, m GroupMemberRow) error {
	return r.queries.InsertGroupMember(ctx, sqlc.InsertGroupMemberParams{
		GroupID:      m.GroupID,
		PlayerID:     m.PlayerID,
		DelayMs:      int64(m.DelayMs),
		VolumeOffset: int64(m.VolumeOffset),
		Position:     int64(m.Position),
	})
}

// UpdateGroupMemberPosition 调整成员在组内的顺序
func (r *PlaybackRepository) UpdateGroupMemberPosition(ctx context.Context, groupID, playerID string, position int) error {
	return r.queries.UpdateGroupMemberPosition(ctx, sqlc.UpdateGroupMemberPositionParams{
		Position: int64(position),
		GroupID:  groupID,
		PlayerID: playerID,
	})
}

// RemoveGroupMember 移出成员，未命中返回 ErrNotFound
func (r *PlaybackRepository) RemoveGroupMember(ctx context.Context, groupID, playerID string) error {
	n, err := r.queries.DeleteGroupMember(ctx, sqlc.DeleteGroupMemberParams{GroupID: groupID, PlayerID: playerID})
	if err != nil {
		return fmt.Errorf("remove group member: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearGroupMembers 清空组成员
func (r *PlaybackRepository) ClearGroupMembers(ctx context.Context, groupID string) error {
	return r.queries.DeleteGroupMembers(ctx, groupID)
}

// DetachPlayerFromGroups 把设备从所有组里摘掉（删除设备时用）
func (r *PlaybackRepository) DetachPlayerFromGroups(ctx context.Context, playerID string) error {
	return r.queries.DeleteGroupsOfPlayer(ctx, playerID)
}

// InsertParty 新建派对
func (r *PlaybackRepository) InsertParty(ctx context.Context, id, name, leaderGroupID string) error {
	return r.queries.InsertParty(ctx, sqlc.InsertPartyParams{ID: id, Name: name, LeaderGroupID: leaderGroupID})
}

// GetParty 取派对，未命中返回 ErrNotFound
func (r *PlaybackRepository) GetParty(ctx context.Context, id string) (name, leaderGroupID string, err error) {
	row, err := r.queries.GetParty(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrNotFound
	}
	if err != nil {
		return "", "", fmt.Errorf("get party: %w", err)
	}
	return row.Name, row.LeaderGroupID, nil
}

// UpdatePartyLeader 切换派对主控组
func (r *PlaybackRepository) UpdatePartyLeader(ctx context.Context, id, leaderGroupID string) error {
	return r.queries.UpdatePartyLeader(ctx, sqlc.UpdatePartyLeaderParams{LeaderGroupID: leaderGroupID, ID: id})
}

// DeleteParty 删除派对，未命中返回 ErrNotFound
func (r *PlaybackRepository) DeleteParty(ctx context.Context, id string) error {
	n, err := r.queries.DeleteParty(ctx, id)
	if err != nil {
		return fmt.Errorf("delete party: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListQueue 读队列（返回按位置排好的 song_id）
func (r *PlaybackRepository) ListQueue(ctx context.Context, ownerKind, ownerID string) ([]int64, error) {
	rows, err := r.queries.ListQueue(ctx, sqlc.ListQueueParams{OwnerKind: ownerKind, OwnerID: ownerID})
	if err != nil {
		return nil, fmt.Errorf("list queue: %w", err)
	}
	return rows, nil
}

// ReplaceQueue 用给定 song_id 序列整体重写队列（先删后插，同一事务）。
//
// 队列重排/删除/插入都走这里：position 是连续的物理序号，逐条 UPDATE 会撞主键，
// 整表重写在队列规模（几百条）下代价可忽略，语义也最不容易错。
func (r *PlaybackRepository) ReplaceQueue(ctx context.Context, ownerKind, ownerID string, songIDs []int64) error {
	tx, ok := r.db.(interface {
		BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
	})
	if !ok {
		return errors.New("playback repository: underlying DBTX does not support transactions")
	}
	t, err := tx.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin queue tx: %w", err)
	}
	defer func() { _ = t.Rollback() }()

	q := sqlc.New(t)
	if err := q.DeleteQueue(ctx, sqlc.DeleteQueueParams{OwnerKind: ownerKind, OwnerID: ownerID}); err != nil {
		return fmt.Errorf("clear queue: %w", err)
	}
	for i, songID := range songIDs {
		if err := q.InsertQueueItem(ctx, sqlc.InsertQueueItemParams{
			OwnerKind: ownerKind,
			OwnerID:   ownerID,
			Position:  int64(i),
			SongID:    songID,
		}); err != nil {
			return fmt.Errorf("insert queue item %d: %w", i, err)
		}
	}
	if err := t.Commit(); err != nil {
		return fmt.Errorf("commit queue tx: %w", err)
	}
	return nil
}

// GetState 读播放状态，未命中返回 ErrNotFound
func (r *PlaybackRepository) GetState(ctx context.Context, ownerKind, ownerID string) (PlaybackStateRow, error) {
	row, err := r.queries.GetState(ctx, sqlc.GetStateParams{OwnerKind: ownerKind, OwnerID: ownerID})
	if errors.Is(err, sql.ErrNoRows) {
		return PlaybackStateRow{}, ErrNotFound
	}
	if err != nil {
		return PlaybackStateRow{}, fmt.Errorf("get state: %w", err)
	}
	return PlaybackStateRow{
		OwnerKind:  row.OwnerKind,
		OwnerID:    row.OwnerID,
		SongID:     row.SongID,
		Position:   row.Position,
		State:      row.State,
		RepeatMode: row.RepeatMode,
		Shuffle:    row.Shuffle != 0,
		UpdatedAt:  row.UpdatedAt,
	}, nil
}

// SaveState 写入播放状态快照
func (r *PlaybackRepository) SaveState(ctx context.Context, s PlaybackStateRow) error {
	if s.UpdatedAt.IsZero() {
		s.UpdatedAt = time.Now()
	}
	if s.RepeatMode == "" {
		s.RepeatMode = "off"
	}
	if s.State == "" {
		s.State = "idle"
	}
	return r.queries.UpsertState(ctx, sqlc.UpsertStateParams{
		OwnerKind:  s.OwnerKind,
		OwnerID:    s.OwnerID,
		SongID:     s.SongID,
		Position:   s.Position,
		State:      s.State,
		RepeatMode: s.RepeatMode,
		Shuffle:    boolToInt64(s.Shuffle),
		UpdatedAt:  s.UpdatedAt,
	})
}

// DeleteState 删除播放状态
func (r *PlaybackRepository) DeleteState(ctx context.Context, ownerKind, ownerID string) error {
	return r.queries.DeleteState(ctx, sqlc.DeleteStateParams{OwnerKind: ownerKind, OwnerID: ownerID})
}
