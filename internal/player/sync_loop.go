package player

import (
	"context"
	"time"

	"songloft/internal/player/sink"
)

// Corrector 同步校正的执行者（由 pipeline.Distributor 实现）。
//
// Skip：丢掉该设备接下来 d 时长的音频，让它"追上"别人；
// Pad：往该设备的流里插 d 时长的静音，让它"等一等"。
// 两者都只影响这一路，**不动时间轴、不暂停解码器**。
type Corrector interface {
	Skip(playerID string, d time.Duration) error
	Pad(playerID string, d time.Duration) error
}

// 校正环参数，对齐 LMS StreamingController::_CheckSync。
const (
	// DefaultSyncInterval 采样间隔（sync_check_interval_ms）。
	DefaultSyncInterval = 950 * time.Millisecond
	// DefaultMinSyncAdjust 小于该偏差不动，避免每轮都做无谓的微调（min_sync_adjust_ms）。
	DefaultMinSyncAdjust = 10 * time.Millisecond
	// DefaultMaxSyncDeviation 偏差超过该值说明设备已经跑飞，不再用丢帧/插静音硬拽。
	DefaultMaxSyncDeviation = 10 * time.Second
)

// SyncAction 一轮校正对某台设备做的动作。
type SyncAction string

const (
	// SyncNone 偏差在容差内，不动。
	SyncNone SyncAction = "none"
	// SyncSkip 该设备落后，丢帧追赶。
	SyncSkip SyncAction = "skip"
	// SyncPad 该设备超前，插静音等一等。
	SyncPad SyncAction = "pad"
	// SyncResync 偏差超限，需要重新挂载该设备。
	SyncResync SyncAction = "resync"
)

// SyncAdjustment 一轮校正的结果（也用于事件与日志）。
type SyncAdjustment struct {
	PlayerID string
	// Delta 该设备相对参考设备的偏差：正=超前，负=落后。
	Delta  time.Duration
	Action SyncAction
	// Applied 实际下发的校正量。
	Applied time.Duration
}

// SyncLoop 组内同步校正环。
//
// 单解码源 + 统一时间轴只保证"同一份音频在同一个绝对时刻发声"，
// 但设备侧的晶振与缓冲各不相同，播久了必然漂开。LMS 靠每 950ms 的
// _CheckSync 兜住这个漂移，这里做同一件事：
//   - 参考设备 = 组内**最落后**的成员（只能让别人等它，不能让它等别人）；
//   - 偏差 < MinAdjust 不动；> MaxDeviation 放弃（重新挂载）；
//   - 落后 → Skip 丢帧；超前 → Pad 插静音（与 LMS skipAhead / pauseForInterval 同构）。
//
// 没有回传通道的 sink（RAOP 之外的纯推送设备若不上报位置）不参与校正。
type SyncLoop struct {
	Interval     time.Duration
	MinAdjust    time.Duration
	MaxDeviation time.Duration
	Targets      *sink.Set
	Timeline     *Timeline
	Corrector    Corrector
	Clock        func() time.Time
	// OnAdjust 每次实际下发校正时回调（偏差在容差内的不回调）。
	OnAdjust func(SyncAdjustment)
	// OnDeviation 设备跑飞时回调，调用方应重新挂载该设备。
	OnDeviation func(playerID string, delta time.Duration)
}

func (l *SyncLoop) interval() time.Duration {
	if l.Interval > 0 {
		return l.Interval
	}
	return DefaultSyncInterval
}

func (l *SyncLoop) minAdjust() time.Duration {
	if l.MinAdjust > 0 {
		return l.MinAdjust
	}
	return DefaultMinSyncAdjust
}

func (l *SyncLoop) maxDeviation() time.Duration {
	if l.MaxDeviation > 0 {
		return l.MaxDeviation
	}
	return DefaultMaxSyncDeviation
}

func (l *SyncLoop) now() time.Time {
	if l.Clock != nil {
		return l.Clock()
	}
	return time.Now()
}

// Run 每 Interval 采样校正一次，ctx 结束即退出。
func (l *SyncLoop) Run(ctx context.Context) {
	t := time.NewTicker(l.interval())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			l.CheckOnce()
		}
	}
}

// CheckOnce 采一轮各成员位置并下发校正，返回本轮的判定（含未超容差的）。
func (l *SyncLoop) CheckOnce() []SyncAdjustment {
	if l.Targets == nil || l.Corrector == nil || l.Timeline == nil {
		return nil
	}
	targets, _ := l.Targets.Snapshot()
	if len(targets) < 2 {
		return nil
	}

	type sample struct {
		target sink.Target
		pos    time.Duration
	}
	samples := make([]sample, 0, len(targets))
	for _, t := range targets {
		rep, ok := t.Sink.(sink.PositionReporter)
		if !ok {
			continue
		}
		pos, ok := rep.PlaybackPosition()
		if !ok {
			continue
		}
		// 补回该设备自己的延迟补偿：delay 越大，它"应该"越落后，折算成同一把尺子。
		samples = append(samples, sample{target: t, pos: pos + t.Delay})
	}
	if len(samples) < 2 {
		return nil
	}

	// 参考设备 = 最落后的一台。
	ref := samples[0].pos
	for _, s := range samples[1:] {
		if s.pos < ref {
			ref = s.pos
		}
	}

	out := make([]SyncAdjustment, 0, len(samples))
	for _, s := range samples {
		delta := s.pos - ref
		adj := SyncAdjustment{PlayerID: s.target.PlayerID, Delta: delta, Action: SyncNone}
		switch {
		case delta > l.maxDeviation() || delta < -l.maxDeviation():
			adj.Action = SyncResync
			if l.OnDeviation != nil {
				l.OnDeviation(s.target.PlayerID, delta)
			}
		case delta >= l.minAdjust():
			adj.Action = SyncPad
			adj.Applied = delta
			_ = l.Corrector.Pad(s.target.PlayerID, adj.Applied)
		case delta <= -l.minAdjust():
			adj.Action = SyncSkip
			adj.Applied = -delta
			_ = l.Corrector.Skip(s.target.PlayerID, adj.Applied)
		}
		if adj.Action != SyncNone && l.OnAdjust != nil {
			l.OnAdjust(adj)
		}
		out = append(out, adj)
	}
	return out
}
