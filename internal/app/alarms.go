package app

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"songloft/internal/database"
	"songloft/internal/player"
)

// alarmRunner 是服务端闹钟的调度器。
//
// 设计取舍：
//   - 时间按"本地时刻 + 星期掩码"存，不做绝对时间换算（见 0042 迁移注释）。
//   - 15 秒扫一次而不是精确 sleep 到点：闹钟精度到分钟就够，定时器也不会
//     因为改系统时间 / 休眠唤醒而漂移；last_fired_at 保证同一分钟只触发一次。
//   - 触发动作是"在目标组上起播一个歌单 + 音量渐强"。渐强由调度器逐步下发
//     音量实现，引擎侧不需要为闹钟加一套单独的增益曲线。
type alarmRunner struct {
	repo     *database.AlarmRepository
	mgr      *player.Manager
	songIDs  func(ctx context.Context, playlistID int64) ([]int64, error)
	now      func() time.Time
	interval time.Duration
	logger   *slog.Logger
}

func newAlarmRunner(repo *database.AlarmRepository, mgr *player.Manager,
	songIDs func(ctx context.Context, playlistID int64) ([]int64, error)) *alarmRunner {
	return &alarmRunner{
		repo:     repo,
		mgr:      mgr,
		songIDs:  songIDs,
		now:      time.Now,
		interval: 15 * time.Second,
		logger:   slog.Default(),
	}
}

// Start 后台跑调度循环，直到 ctx 结束。
func (r *alarmRunner) Start(ctx context.Context) {
	if r == nil || r.repo == nil || r.mgr == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.tick(ctx)
			}
		}
	}()
}

// tick 扫一遍到点的闹钟。单个闹钟失败不影响其它闹钟。
func (r *alarmRunner) tick(ctx context.Context) {
	alarms, err := r.repo.List(ctx)
	if err != nil {
		r.logger.Warn("读取闹钟列表失败", "err", err)
		return
	}
	now := r.now()
	for _, a := range alarms {
		if !alarmDue(a, now) {
			continue
		}
		if err := r.fire(ctx, a); err != nil {
			r.logger.Warn("闹钟触发失败", "alarm_id", a.ID, "name", a.Name, "err", err)
			continue
		}
		r.logger.Info("闹钟已触发", "alarm_id", a.ID, "name", a.Name,
			"target_id", a.TargetID, "playlist_id", a.PlaylistID)
	}
}

// Trigger 立即触发指定闹钟（调试 / "试听"按钮用，不写 last_fired_at）。
func (r *alarmRunner) Trigger(ctx context.Context, id string) error {
	a, err := r.repo.Get(ctx, id)
	if err != nil {
		return err
	}
	return r.fire(ctx, a)
}

// fire 在目标组上起播歌单并把音量渐强到设定值。
func (r *alarmRunner) fire(ctx context.Context, a *database.AlarmRow) error {
	groupID := a.TargetID
	if gid := r.mgr.GroupIDOf(a.TargetID); gid != "" {
		groupID = gid
	}
	eng, err := r.mgr.Engine(groupID)
	if err != nil {
		return fmt.Errorf("取目标组 %q 的引擎失败: %w", groupID, err)
	}

	if a.PlaylistID > 0 {
		ids, err := r.songIDs(ctx, a.PlaylistID)
		if err != nil {
			return fmt.Errorf("读取歌单 %d 失败: %w", a.PlaylistID, err)
		}
		if len(ids) == 0 {
			return fmt.Errorf("歌单 %d 没有歌曲", a.PlaylistID)
		}
		if err := eng.Load(ctx, ids, 0, 0); err != nil {
			return fmt.Errorf("起播失败: %w", err)
		}
	}

	// 渐强：先压到目标的一小部分，再逐级拉上去。fade_seconds=0 直接设目标音量。
	steps := fadeVolumeSteps(a.Volume, a.FadeSeconds)
	if len(steps) > 0 {
		_, _ = r.mgr.SetGroupVolume(ctx, groupID, steps[0], false)
		go r.rampVolume(groupID, steps[1:])
	} else {
		_, _ = r.mgr.SetGroupVolume(ctx, groupID, a.Volume, false)
	}

	// 起播之后再记 fired，失败（比如组不存在）不会把当天的闹钟吞掉。
	if err := r.repo.MarkFired(ctx, a.ID, alarmMinuteKey(r.now())); err != nil {
		r.logger.Warn("记录闹钟触发时间失败", "alarm_id", a.ID, "err", err)
	}
	return nil
}

// rampVolume 每秒下发一级音量。用独立的 context，不跟请求 / 调度 ctx 绑定，
// 否则调度循环一返回，渐强就断了。
func (r *alarmRunner) rampVolume(groupID string, steps []int) {
	for _, v := range steps {
		time.Sleep(time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if _, err := r.mgr.SetGroupVolume(ctx, groupID, v, false); err != nil {
			cancel()
			return
		}
		cancel()
	}
}

// alarmDue 判断闹钟是否该在这一分钟内触发。
//
// DaysMask 为 0 表示"只在今天触发一次"：此时不看星期，靠 last_fired_at
// 保证只响一次（响过就再也不会匹配）。
func alarmDue(a *database.AlarmRow, now time.Time) bool {
	if a == nil || !a.Enabled {
		return false
	}
	h, m, ok := parseHHMM(a.TimeOfDay)
	if !ok {
		return false
	}
	if now.Hour() != h || now.Minute() != m {
		return false
	}
	if a.DaysMask != 0 {
		bit := 1 << uint((int(now.Weekday())+6)%7) // 周一=bit0 … 周日=bit6
		if a.DaysMask&bit == 0 {
			return false
		}
	}
	return a.LastFiredAt != alarmMinuteKey(now)
}

// alarmMinuteKey 把时间压成分钟粒度字符串，用于 last_fired_at 去重。
func alarmMinuteKey(t time.Time) string {
	return t.Format("2006-01-02T15:04")
}

func parseHHMM(s string) (int, int, bool) {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		return 0, 0, false
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// fadeVolumeSteps 返回从起点到目标音量的逐级序列（含起点与终点）。
// 每级约 1 秒；fade 0 秒时返回空切片，调用方直接设目标音量。
func fadeVolumeSteps(target, fadeSeconds int) []int {
	if fadeSeconds <= 0 || target <= 0 {
		return nil
	}
	steps := fadeSeconds
	if steps > target { // 别让音量在 0 上停留太久，也别超过目标级数
		steps = target
	}
	if steps <= 0 {
		return nil
	}
	start := target / 5
	if start < 1 {
		start = 1
	}
	out := make([]int, 0, steps+1)
	for i := 0; i <= steps; i++ {
		v := start + (target-start)*i/steps
		out = append(out, v)
	}
	return out
}
