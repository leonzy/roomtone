package player

import (
	"sync"
	"time"
)

// Timeline 组内统一时间轴。
//
// 组内只有一个解码源，每帧在时间轴上有唯一的绝对采样序号 n；
// 各 sink 拿到同一份 (n, play_at) 后按自己的协议时钟发声，
// 因此同步精度只取决于协议，与 Go 侧调度无关。
//
// 时钟用 time.Now() 的单调读数，**不用墙钟**——NTP 跳变会撕裂时间轴。
// 可注入是为了让定时逻辑能确定性测试。
type Timeline struct {
	mu         sync.Mutex
	t0         time.Time
	sampleRate int
	anchor     uint32
	latency    time.Duration
}

// NewTimeline 创建时间轴。latency 是起播提前量（RAOP 默认 2s）。
func NewTimeline(now time.Time, sampleRate int, latency time.Duration) *Timeline {
	if sampleRate <= 0 {
		sampleRate = 44100
	}
	return &Timeline{t0: now, sampleRate: sampleRate, latency: latency}
}

// At 第 n 个采样帧的绝对播放时刻。
func (t *Timeline) At(n uint64) time.Time {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.atLocked(n)
}

func (t *Timeline) atLocked(n uint64) time.Time {
	offset := time.Duration(float64(n) / float64(t.sampleRate) * float64(time.Second))
	return t.t0.Add(offset).Add(t.latency)
}

// SampleAt 某个墙钟时刻对应的采样序号（用于 seek / 漂移校正）。
func (t *Timeline) SampleAt(wall time.Time) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := wall.Sub(t.t0) - t.latency
	if d <= 0 {
		return 0
	}
	return uint64(d.Seconds() * float64(t.sampleRate))
}

// SampleRate 采样率。
func (t *Timeline) SampleRate() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sampleRate
}

// Latency 起播提前量。
func (t *Timeline) Latency() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.latency
}

// Reset 重建时间轴（seek 或切歌重建解码器时用）。
func (t *Timeline) Reset(now time.Time, latency time.Duration) {
	t.mu.Lock()
	t.t0 = now
	t.latency = latency
	t.mu.Unlock()
}

// Anchor 起播 RTP 时间戳（RAOP 用，随机化避免与别的流撞）。
func (t *Timeline) Anchor() uint32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.anchor
}

// SetAnchor 设置 RTP 基准。
func (t *Timeline) SetAnchor(a uint32) {
	t.mu.Lock()
	t.anchor = a
	t.mu.Unlock()
}

// RTPTime 第 n 个采样帧对应的 32 位 RTP 时间戳。
func (t *Timeline) RTPTime(n uint64) uint32 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.anchor + uint32(n)
}

// NTPTime 把时间轴上的时刻换算成 NTP 64 位定点（RAOP sync 包用）。
// 返回秒的高 32 位与小数部分。
func NTPTime(wall time.Time) (sec uint32, frac uint32) {
	const ntpEpochOffset = 2208988800 // 1900-01-01 到 1970-01-01 的秒数
	sec = uint32(wall.Unix() + ntpEpochOffset)
	frac = uint32(float64(wall.Nanosecond()) / 1e9 * 4294967296.0)
	return sec, frac
}
