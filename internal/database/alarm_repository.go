package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"songloft/internal/database/sqlc"
)

// AlarmRepository 闹钟仓储。
//
// 全部是固定 SQL，只有 List/Get/Create/Update/Delete/MarkFired 六个动作，
// 没有变长 WHERE，所以不引入 squirrel。
type AlarmRepository struct {
	db sqlc.DBTX
}

// NewAlarmRepository 创建闹钟仓储。
func NewAlarmRepository(db sqlc.DBTX) *AlarmRepository {
	return &AlarmRepository{db: db}
}

// AlarmRow 闹钟行。
//
// TimeOfDay 是本地时刻 "HH:MM"；DaysMask 按 bit0=周一 … bit6=周日，
// 0 表示不重复（只在下次匹配时触发一次，前端用一次性闹钟就是今天那一档）。
type AlarmRow struct {
	ID          string
	Name        string
	TargetID    string
	TimeOfDay   string
	DaysMask    int
	PlaylistID  int64
	Volume      int
	FadeSeconds int
	Enabled     bool
	LastFiredAt string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

const alarmColumns = `id, name, target_id, time_of_day, days_mask, playlist_id,
	volume, fade_seconds, enabled, last_fired_at, created_at, updated_at`

func scanAlarm(row interface{ Scan(...any) error }) (*AlarmRow, error) {
	var a AlarmRow
	var enabled int
	var created, updated string
	if err := row.Scan(&a.ID, &a.Name, &a.TargetID, &a.TimeOfDay, &a.DaysMask,
		&a.PlaylistID, &a.Volume, &a.FadeSeconds, &enabled, &a.LastFiredAt,
		&created, &updated); err != nil {
		return nil, err
	}
	a.Enabled = enabled != 0
	a.CreatedAt, _ = parseAlarmTime(created)
	a.UpdatedAt, _ = parseAlarmTime(updated)
	return &a, nil
}

// SQLite 的 datetime('now') 是 "2006-01-02 15:04:05"，这里容忍两种格式。
func parseAlarmTime(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02 15:04:05", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time %q", s)
}

// List 返回全部闹钟，按触发时刻排序（前端直接渲染，不用再排）。
func (r *AlarmRepository) List(ctx context.Context) ([]*AlarmRow, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+alarmColumns+` FROM alarms ORDER BY time_of_day, name`)
	if err != nil {
		return nil, fmt.Errorf("list alarms: %w", err)
	}
	defer rows.Close()

	out := []*AlarmRow{}
	for rows.Next() {
		a, err := scanAlarm(rows)
		if err != nil {
			return nil, fmt.Errorf("scan alarm: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Get 按 id 取闹钟，未命中返回 ErrNotFound。
func (r *AlarmRepository) Get(ctx context.Context, id string) (*AlarmRow, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+alarmColumns+` FROM alarms WHERE id = ?`, id)
	a, err := scanAlarm(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get alarm: %w", err)
	}
	return a, nil
}

// Create 插入闹钟。
func (r *AlarmRepository) Create(ctx context.Context, a *AlarmRow) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO alarms (id, name, target_id, time_of_day, days_mask, playlist_id,
			volume, fade_seconds, enabled, last_fired_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.Name, a.TargetID, a.TimeOfDay, a.DaysMask, a.PlaylistID,
		a.Volume, a.FadeSeconds, boolToInt(a.Enabled), a.LastFiredAt)
	if err != nil {
		return fmt.Errorf("create alarm: %w", err)
	}
	return nil
}

// Update 覆盖闹钟的可编辑字段（id 与 last_fired_at 不变）。
func (r *AlarmRepository) Update(ctx context.Context, a *AlarmRow) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE alarms SET name = ?, target_id = ?, time_of_day = ?, days_mask = ?,
			playlist_id = ?, volume = ?, fade_seconds = ?, enabled = ?,
			updated_at = datetime('now')
		 WHERE id = ?`,
		a.Name, a.TargetID, a.TimeOfDay, a.DaysMask, a.PlaylistID,
		a.Volume, a.FadeSeconds, boolToInt(a.Enabled), a.ID)
	if err != nil {
		return fmt.Errorf("update alarm: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete 删除闹钟。
func (r *AlarmRepository) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM alarms WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete alarm: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkFired 记录本次触发到的分钟，用于去重（同一分钟不重复起播）。
func (r *AlarmRepository) MarkFired(ctx context.Context, id, minute string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE alarms SET last_fired_at = ? WHERE id = ?`, minute, id)
	if err != nil {
		return fmt.Errorf("mark alarm fired: %w", err)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
