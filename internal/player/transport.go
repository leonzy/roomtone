package player

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// State 传输状态。
type State string

const (
	StateIdle      State = "idle"
	StateLoading   State = "loading"
	StateBuffering State = "buffering"
	StatePlaying   State = "playing"
	StatePaused    State = "paused"
	StateError     State = "error"
)

// ErrInvalidTransition 非法状态迁移。
var ErrInvalidTransition = errors.New("invalid transport transition")

// DefaultBufferWatermark buffering→playing 的判据：环形缓冲水位达到 1s。
// **不是**"第一帧到达"——那会让组内各设备从不同水位起播，同步从一开始就散。
//
// 必须**严格小于**分发器的推送深度（pipeline.BufferDepth，默认 2s）：水位是
// "已推送但还没到播放时刻"的时长，推送深度把它压在 2s 以内，判据一旦取到
// 2s 就永远差一点点顶不到，播放会一直卡在 buffering（实测停在 1.9998s）。
const DefaultBufferWatermark = time.Second

// MinBufferWatermark 最低水位，低于这个值起播必然立刻欠载。
const MinBufferWatermark = 500 * time.Millisecond

// Transport 传输状态机 + 逻辑播放位置。
//
// 状态迁移：
//
//	idle ──load──► loading ──► buffering ──► playing ⇄ paused
//	                 │            │            │
//	                 └────────────┴────────────┴──► error ──recover──► idle
//
// 位置只在 playing 时前进（monotonic 时钟），pause/seek 会重置基准。
type Transport struct {
	mu        sync.Mutex
	state     State
	errMsg    string
	watermark time.Duration
	target    time.Duration
	base      time.Duration // 上一次状态切换时的位置
	since     time.Time     // 进入 playing 的时刻
	songID    int64
	now       func() time.Time
}

// NewTransport 创建状态机。
func NewTransport() *Transport {
	return &Transport{state: StateIdle, target: DefaultBufferWatermark, now: time.Now}
}

// SetClock 注入时钟（测试用）。
func (t *Transport) SetClock(now func() time.Time) {
	t.mu.Lock()
	t.now = now
	t.mu.Unlock()
}

// SetWatermarkTarget 调整起播水位，低于 MinBufferWatermark 时钳到下限。
func (t *Transport) SetWatermarkTarget(d time.Duration) {
	if d < MinBufferWatermark {
		d = MinBufferWatermark
	}
	t.mu.Lock()
	t.target = d
	t.mu.Unlock()
}

// State 当前状态。
func (t *Transport) State() State {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

// Error 错误原因（StateError 时有值）。
func (t *Transport) Error() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.errMsg
}

// SongID 当前歌曲。
func (t *Transport) SongID() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.songID
}

// Load 开始加载某首歌。position 是起始位置（seek 到中间时非零）。
func (t *Transport) Load(songID int64, position time.Duration) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch t.state {
	case StateIdle, StateLoading, StateBuffering, StatePlaying, StatePaused, StateError:
		t.state = StateLoading
		t.errMsg = ""
		t.songID = songID
		t.base = position
		t.since = time.Time{}
		t.watermark = 0
		return nil
	}
	return fmt.Errorf("%w: load from %s", ErrInvalidTransition, t.state)
}

// MarkBuffering 解码器已开跑，等缓冲。
func (t *Transport) MarkBuffering() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != StateLoading {
		return fmt.Errorf("%w: buffering from %s", ErrInvalidTransition, t.state)
	}
	t.state = StateBuffering
	t.watermark = 0
	return nil
}

// ReportWatermark 上报环形缓冲水位。
//
// 只有 buffering 状态且水位达到目标才迁到 playing；
// 其它状态下调用是无害的（解码器按帧节奏上报，不必额外判断状态）。
func (t *Transport) ReportWatermark(d time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if d > t.watermark {
		t.watermark = d
	}
	if t.state == StateBuffering && t.watermark >= t.target {
		t.state = StatePlaying
		t.since = t.now()
		return true
	}
	return false
}

// Watermark 当前水位。
func (t *Transport) Watermark() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.watermark
}

// Play 从暂停恢复（或 buffering 已完成时直接播放）。
func (t *Transport) Play() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch t.state {
	case StatePlaying:
		return nil
	case StatePaused, StateBuffering:
		t.state = StatePlaying
		t.since = t.now()
		return nil
	case StateLoading:
		// 还在加载：忽略，等缓冲水位到达自然起播。
		return nil
	}
	return fmt.Errorf("%w: play from %s", ErrInvalidTransition, t.state)
}

// Pause 暂停，冻结位置。
func (t *Transport) Pause() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch t.state {
	case StatePaused:
		return nil
	case StatePlaying:
		t.base = t.positionLocked()
		t.since = time.Time{}
		t.state = StatePaused
		return nil
	case StateBuffering:
		t.state = StatePaused
		return nil
	}
	return fmt.Errorf("%w: pause from %s", ErrInvalidTransition, t.state)
}

// RestorePaused 直接进入暂停态（服务重启后恢复用）。
//
// 不能走 Load→Pause：Pause 不接受 loading，会返回 ErrInvalidTransition 并把
// 状态机永久留在 loading；而 Play 在 loading 下是空操作，区域就此卡死。
func (t *Transport) RestorePaused(songID int64, position time.Duration) {
	t.mu.Lock()
	t.state = StatePaused
	t.errMsg = ""
	t.songID = songID
	t.base = position
	t.since = time.Time{}
	t.watermark = 0
	t.mu.Unlock()
}

// SelectSong 只改当前曲目，不动状态机。
//
// 客户端推队列时用：区域应该显示队列里的当前曲目，但推队列本身不代表
// 开始播放，所以不能把状态改成 paused/playing。
// SelectSong 只换当前曲目，不动播放状态。
//
// 同一首歌重复选中必须是 no-op：队列编辑（PUT /groups/{id}/queue）每次
// 都会走到这里，如果无条件把 base 归零，正在播的歌位置会被清零，切区域
// 回来就“丢进度”（服务端驱动时 MirrorReport 被忽略，位置永远回不来）。
func (t *Transport) SelectSong(songID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.songID == songID {
		return
	}
	t.songID = songID
	t.base = 0
	t.since = time.Time{}
	t.watermark = 0
}

// Mirror 按客户端上报直接设定状态（服务端未驱动时使用）。
//
// local sink 是客户端权威的：用户直接点客户端播放条时服务端既没下发指令也
// 没有解码协程，组状态会一直停在 idle。这里把上报的状态搬进状态机，让区域
// 显示与真实出声一致。
func (t *Transport) Mirror(songID int64, position time.Duration, playing bool) {
	t.mu.Lock()
	t.errMsg = ""
	if songID > 0 {
		t.songID = songID
	}
	t.base = position
	t.watermark = 0
	if playing {
		t.state = StatePlaying
		t.since = t.now()
	} else {
		t.state = StatePaused
		t.since = time.Time{}
	}
	t.mu.Unlock()
}

// Stop 回到 idle 并清空位置。
func (t *Transport) Stop() {
	t.mu.Lock()
	t.state = StateIdle
	t.errMsg = ""
	t.base = 0
	t.since = time.Time{}
	t.songID = 0
	t.watermark = 0
	t.mu.Unlock()
}

// Seek 跳转到 position。播放中的 seek 保留播放状态，位置基准重置。
func (t *Transport) Seek(position time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if position < 0 {
		position = 0
	}
	t.base = position
	if t.state == StatePlaying {
		t.since = t.now()
	}
}

// Fail 进入错误态。
func (t *Transport) Fail(err error) {
	t.mu.Lock()
	t.state = StateError
	if err != nil {
		t.errMsg = err.Error()
	}
	t.since = time.Time{}
	t.mu.Unlock()
}

// Recover 从错误态恢复。
func (t *Transport) Recover() {
	t.mu.Lock()
	if t.state == StateError {
		t.state = StateIdle
		t.errMsg = ""
		t.base = 0
		// idle 表示"没有任何曲目在播"，songID 必须一起清掉，
		// 否则 UI 会看到 idle 状态却仍报着上一首。
		t.songID = 0
		t.watermark = 0
	}
	t.mu.Unlock()
}

// Position 当前逻辑位置。
func (t *Transport) Position() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.positionLocked()
}

func (t *Transport) positionLocked() time.Duration {
	if t.state == StatePlaying && !t.since.IsZero() {
		return t.base + t.now().Sub(t.since)
	}
	return t.base
}
