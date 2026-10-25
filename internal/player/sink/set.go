package sink

import (
	"sync"
	"time"
)

// Target 一路推送目标：sink 本身 + 该路独立的处理与补偿参数。
//
// 之所以把 EQ 挂在"路"上而不是引擎上：每台音箱的音色诉求不同（书架箱要补低频、
// 吸顶喇叭要削高频），组内共享一条滤镜链做不到。
type Target struct {
	PlayerID string
	Sink     PushSink
	// EQ 该设备独立的均衡器，nil 表示直通。逐样本处理，样本数不变。
	EQ SampleProcessor
	// Delay 该设备的固定延迟补偿（正值 = 比主控晚出声）。起播时刻与校正环都要用。
	Delay time.Duration
}

// SampleProcessor 逐样本处理 PCM（s16le 交错）。实现必须保持样本数不变。
type SampleProcessor interface {
	ProcessInPlace(pcm []byte)
}

// PositionReporter 能报告"当前正在发声的内容位置"的 sink。
//
// 只有具备真实回传通道的设备能实现：本机客户端上报播放进度、DLNA 轮询
// GetPositionInfo。RAOP 没有回传通道，靠 RTP 时间戳由协议保证，不实现该接口。
type PositionReporter interface {
	PlaybackPosition() (time.Duration, bool)
}

// Set 可热替换的推送目标集合。
//
// 播放中加入/移除成员必须"下一帧生效"：引擎持有同一个 Set 指针，
// 分发器每帧读快照，不需要重启解码，也不需要等下一曲。
type Set struct {
	mu      sync.RWMutex
	targets []Target
	version uint64
}

// NewSet 创建集合。
func NewSet(ts ...Target) *Set {
	return &Set{targets: append([]Target(nil), ts...), version: 1}
}

// Snapshot 取当前快照与版本号。版本号用于分发器判断是否需要重建写协程。
func (s *Set) Snapshot() ([]Target, uint64) {
	if s == nil {
		return nil, 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Target(nil), s.targets...), s.version
}

// Version 当前版本号。
func (s *Set) Version() uint64 {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.version
}

// Store 全量替换。
func (s *Set) Store(ts []Target) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.targets = append([]Target(nil), ts...)
	s.version++
	s.mu.Unlock()
}

// Add 加入一路目标。同 PlayerID 已存在时替换并返回 false（表示不是新增）。
func (s *Set) Add(t Target) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.targets {
		if s.targets[i].PlayerID == t.PlayerID {
			s.targets[i] = t
			s.version++
			return false
		}
	}
	s.targets = append(s.targets, t)
	s.version++
	return true
}

// Remove 移除一路目标。
func (s *Set) Remove(playerID string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.targets {
		if s.targets[i].PlayerID == playerID {
			s.targets = append(s.targets[:i], s.targets[i+1:]...)
			s.version++
			return true
		}
	}
	return false
}

// Get 取某路目标。
func (s *Set) Get(playerID string) (Target, bool) {
	if s == nil {
		return Target{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, t := range s.targets {
		if t.PlayerID == playerID {
			return t, true
		}
	}
	return Target{}, false
}

// Len 目标数量。
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.targets)
}

// IDs 目标 id 列表。
func (s *Set) IDs() []string {
	if s == nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.targets))
	for _, t := range s.targets {
		out = append(out, t.PlayerID)
	}
	return out
}
