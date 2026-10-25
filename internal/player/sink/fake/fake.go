// Package fake 是测试用的假 sink。
//
// 它把每一帧的 (SampleIndex, PlayAt, 字节数) 都记下来，
// 断言"20ms 一帧 / 时间戳单调 / 欠载补静音"全靠这份记录。
package fake

import (
	"context"
	"sync"
	"time"

	"songloft/internal/player/sink"
)

// FrameRecord 一帧的观测记录。
type FrameRecord struct {
	SampleIndex int64
	PlayAt      time.Time
	Bytes       int
	Silent      bool
}

// Sink 记录所有帧的假输出端。
type Sink struct {
	id   string
	caps sink.Caps

	mu       sync.Mutex
	frames   []FrameRecord
	plan     sink.StreamPlan
	started  time.Time
	volume   float64
	opened   bool
	stopped  bool
	closed   bool
	writeErr error
	events   chan sink.Event
}

// New 创建假 sink。
func New(id string) *Sink {
	return &Sink{
		id:     id,
		caps:   sink.Caps{Codecs: []sink.Codec{sink.CodecL16}, MaxSampleRate: 44100, Channels: 2, BitDepth: 16, SupportsVolume: true, SupportsSeek: true, LatencyMs: 2000, SyncTier: sink.SyncExact},
		events: make(chan sink.Event, 16),
	}
}

// SetCaps 覆盖能力（测同步降级用）。
func (s *Sink) SetCaps(c sink.Caps) {
	s.mu.Lock()
	s.caps = c
	s.mu.Unlock()
}

// SetWriteError 让后续 WriteFrame 返回错误。
func (s *Sink) SetWriteError(err error) {
	s.mu.Lock()
	s.writeErr = err
	s.mu.Unlock()
}

// ID 实现 sink.Sink。
func (s *Sink) ID() string { return s.id }

// Kind 实现 sink.Sink。
func (s *Sink) Kind() sink.Kind { return sink.KindLocal }

// Caps 实现 sink.Sink。
func (s *Sink) Caps() sink.Caps {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.caps
}

// Open 记录播放计划。
func (s *Sink) Open(_ context.Context, plan sink.StreamPlan) error {
	s.mu.Lock()
	s.plan = plan
	s.opened = true
	s.mu.Unlock()
	s.events <- sink.Event{Type: sink.EventBuffering}
	return nil
}

// Start 记录起播时刻。
func (s *Sink) Start(at time.Time) error {
	s.mu.Lock()
	s.started = at
	s.mu.Unlock()
	s.events <- sink.Event{Type: sink.EventPlaying}
	return nil
}

// Seek 实现 sink.Sink（假实现，只记录）。
func (s *Sink) Seek(sample uint64) error {
	s.mu.Lock()
	s.frames = append(s.frames, FrameRecord{SampleIndex: int64(sample), PlayAt: time.Now(), Silent: true})
	s.mu.Unlock()
	return nil
}

// SetVolume 实现 sink.Sink。
func (s *Sink) SetVolume(v float64) error {
	s.mu.Lock()
	s.volume = v
	s.mu.Unlock()
	return nil
}

// Stop 实现 sink.Sink。
func (s *Sink) Stop() error {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	return nil
}

// Close 实现 sink.Sink。
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

// IsStopped 是否被 Stop 过（测试断言用）。
func (s *Sink) IsStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

// IsClosed 是否已被 Close（测试断言用）。
func (s *Sink) IsClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Events 实现 sink.Sink。
func (s *Sink) Events() <-chan sink.Event { return s.events }

// WriteFrame 记录一帧。
func (s *Sink) WriteFrame(f sink.Frame) error {
	s.mu.Lock()
	err := s.writeErr
	silent := true
	for _, b := range f.Data {
		if b != 0 {
			silent = false
			break
		}
	}
	s.frames = append(s.frames, FrameRecord{SampleIndex: f.SampleIndex, PlayAt: f.PlayAt, Bytes: len(f.Data), Silent: silent})
	s.mu.Unlock()
	return err
}

// Frames 返回帧记录副本。
func (s *Sink) Frames() []FrameRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]FrameRecord, len(s.frames))
	copy(out, s.frames)
	return out
}

// FrameCount 帧数。
func (s *Sink) FrameCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.frames)
}

// SilentFrames 静音帧数（欠载补的）。
func (s *Sink) SilentFrames() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, f := range s.frames {
		if f.Silent {
			n++
		}
	}
	return n
}

// Volume 最后一次设置的音量。
func (s *Sink) Volume() float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.volume
}

// Plan 播放计划。
func (s *Sink) Plan() sink.StreamPlan {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.plan
}

// StartedAt 起播时刻。
func (s *Sink) StartedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// Opened 是否被 Open 过。
func (s *Sink) Opened() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.opened
}
