// Package local 是自研客户端（Flutter / Lynx）的输出端。
//
// 它不接收 PCM：服务端只下发"播什么、从哪开始、什么时候开始"，
// 客户端自己拉 /songs/{id}/stream 播放。好处是不引入任何外部协议风险，
// 却能让"手机 + 客厅音箱"立刻同组（§5.2）。
//
// 时钟对齐：服务端给绝对时刻 play_at（Unix 毫秒），客户端按
// (play_at - 本地时钟偏移) 起播；偏移用一次 RTT 折半估算。
package local

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"songloft/internal/player/sink"
)

// Controller 把指令发给某个客户端连接（WS hub 实现）。
//
// Send 必须是非阻塞的：客户端卡住不能拖慢整组的分发循环。
type Controller interface {
	SendCommand(cmd Command) error
}

// Command 下发给客户端的一条指令。
type Command struct {
	Cmd        string `json:"cmd"`
	SongID     int64  `json:"song_id,omitempty"`
	PositionMs int64  `json:"position_ms,omitempty"`
	PlayAtMs   int64  `json:"play_at_ms,omitempty"`
	Volume     int    `json:"volume,omitempty"`
	Muted      bool   `json:"muted,omitempty"`
	Format     string `json:"format,omitempty"`
}

// Report 客户端每 500ms 上报的状态。
type Report struct {
	PositionMs int64  `json:"position_ms"`
	Buffering  bool   `json:"buffering"`
	Volume     int    `json:"volume"`
	State      string `json:"state"`
	// RTTMs 客户端测得的往返时延，服务端据此估算时钟偏移（折半）。
	RTTMs int64 `json:"rtt_ms"`
}

// Sink local 输出端。
type Sink struct {
	id      string
	caps    sink.Caps
	ctl     Controller
	events  chan sink.Event
	mu      sync.Mutex
	plan    sink.StreamPlan
	volume  int
	muted   bool
	started time.Time
	closed  bool
	// offset 服务端与客户端时钟的偏移估计（客户端时间 - 服务端时间）。
	offset time.Duration
	rtt    time.Duration
}

// New 创建 local sink。
func New(id string, ctl Controller, caps sink.Caps) *Sink {
	if caps.MaxSampleRate <= 0 {
		caps = sink.Caps{
			Codecs:         []sink.Codec{sink.CodecMP3, sink.CodecAAC, sink.CodecFLAC},
			MaxSampleRate:  48000,
			Channels:       2,
			BitDepth:       16,
			SupportsVolume: true,
			SupportsSeek:   true,
			LatencyMs:      300,
			SyncTier:       sink.SyncExact,
		}
	}
	return &Sink{id: id, ctl: ctl, caps: caps, events: make(chan sink.Event, 16), volume: 100}
}

// ID 实现 sink.Sink。
func (s *Sink) ID() string { return s.id }

// Kind 实现 sink.Sink。
func (s *Sink) Kind() sink.Kind { return sink.KindLocal }

// Caps 实现 sink.Sink。
func (s *Sink) Caps() sink.Caps { return s.caps }

// Open 下发 load 指令。
func (s *Sink) Open(_ context.Context, plan sink.StreamPlan) error {
	s.mu.Lock()
	s.plan = plan
	s.mu.Unlock()
	if s.ctl == nil {
		return errors.New("local sink: 客户端连接已断开")
	}
	return s.ctl.SendCommand(Command{
		Cmd:        "load",
		SongID:     plan.SongID,
		PositionMs: plan.Position.Milliseconds(),
		Format:     string(plan.Format.Codec),
	})
}

// Start 下发 play，带绝对起播时刻。
func (s *Sink) Start(at time.Time) error {
	s.mu.Lock()
	s.started = at
	s.mu.Unlock()
	if s.ctl == nil {
		return nil
	}
	return s.ctl.SendCommand(Command{Cmd: "play", PlayAtMs: at.UnixMilli()})
}

// Seek 下发 seek。
func (s *Sink) Seek(sample uint64) error {
	rate := s.plan.Format.SampleRate
	if rate <= 0 {
		rate = 44100
	}
	pos := time.Duration(float64(sample) / float64(rate) * float64(time.Second))
	if s.ctl == nil {
		return nil
	}
	return s.ctl.SendCommand(Command{Cmd: "seek", PositionMs: pos.Milliseconds()})
}

// SetVolume 下发音量。
func (s *Sink) SetVolume(v float64) error {
	s.mu.Lock()
	s.volume = int(v * 100)
	s.mu.Unlock()
	if s.ctl == nil {
		return nil
	}
	return s.ctl.SendCommand(Command{Cmd: "volume", Volume: s.volume})
}

// Pause 下发暂停（引擎停掉解码后调用，客户端保留本地缓冲）。
func (s *Sink) Pause() error {
	if s.ctl == nil {
		return nil
	}
	return s.ctl.SendCommand(Command{Cmd: "pause"})
}

// Stop 下发 stop。
func (s *Sink) Stop() error {
	if s.ctl == nil {
		return nil
	}
	return s.ctl.SendCommand(Command{Cmd: "stop"})
}

// Close 关闭。
func (s *Sink) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()
	close(s.events)
	return nil
}

// Events 实现 sink.Sink。
func (s *Sink) Events() <-chan sink.Event { return s.events }

// HandleReport 处理客户端上报。
//
// 上报里的 position 与本地时钟差用来修正 offset：客户端说"我现在在 12.3s"，
// 服务端知道"按我的时钟此刻应该是 12.35s"，差值是可观测的偏移。
// 这里只用 RTT 折半做一次粗估，精确校正交给 group_drift 的持续观测。
func (s *Sink) HandleReport(r Report) {
	s.mu.Lock()
	if r.RTTMs > 0 {
		s.rtt = time.Duration(r.RTTMs) * time.Millisecond
		s.offset = s.rtt / 2
	}
	s.mu.Unlock()

	ev := sink.Event{Type: sink.EventProgress, Position: time.Duration(r.PositionMs) * time.Millisecond}
	switch r.State {
	case "playing":
		ev.Type = sink.EventPlaying
	case "paused":
		ev.Type = sink.EventPaused
	case "buffering":
		ev.Type = sink.EventBuffering
	}
	if r.Buffering {
		ev.Type = sink.EventBuffering
	}
	select {
	case s.events <- ev:
	default:
		// 客户端上报是采样性质的，丢一条不影响判断。
	}
}

// Offset 当前时钟偏移估计。
func (s *Sink) Offset() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.offset
}

// ValidateCommand 校验指令名，防止前后端拼错时静默失效。
func ValidateCommand(cmd string) error {
	switch cmd {
	case "load", "play", "pause", "stop", "seek", "volume":
		return nil
	}
	return fmt.Errorf("unknown local command %q", cmd)
}
