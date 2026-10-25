package player

import (
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"songloft/internal/player/sink"
)

// EventType 推送事件类型（对应 docs/architecture_playback.md §7.4）。
type EventType string

const (
	EventState         EventType = "state"
	EventTargetChanged EventType = "target_changed"
	EventMemberChanged EventType = "member_changed"
	// EventGroupChanged 组本身变了：创建 / 删除 / 换主控。action 区分。
	EventGroupChanged EventType = "group_changed"
	EventSpeakerSeen  EventType = "speaker_seen"
	EventPartyChanged EventType = "party_changed"
	EventGroupDrift   EventType = "group_drift"
	EventEQChanged    EventType = "eq_changed"
	EventError        EventType = "error"
)

// Event 推给客户端的一条消息。
//
// 所有事件共用 seq：客户端只需比较 seq 丢旧消息，不用按类型各维护一份。
// 字段用 omitempty，避免每种事件都带一堆 null。
type Event struct {
	Seq    uint64    `json:"seq"`
	Type   EventType `json:"type"`
	TimeMs int64     `json:"time_ms"`

	GroupID     string `json:"group_id,omitempty"`
	FromGroupID string `json:"from_group_id,omitempty"`
	PartyID     string `json:"party_id,omitempty"`
	PlayerID    string `json:"player_id,omitempty"`
	SongID      int64  `json:"song_id,omitempty"`
	Position    int64  `json:"position_ms,omitempty"`
	Status      string `json:"status,omitempty"`
	Action      string `json:"action,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Name        string `json:"name,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Code        string `json:"code,omitempty"`
	Message     string `json:"message,omitempty"`

	// EQ / 漂移专用
	Enabled   *bool  `json:"enabled,omitempty"`
	Preset    string `json:"preset,omitempty"`
	OffsetMs  int    `json:"offset_ms,omitempty"`
	Corrected *bool  `json:"corrected,omitempty"`
	// Unaligned 连续校正失败、已放弃重试（前端应把该组标成"未对齐"）。
	Unaligned *bool `json:"unaligned,omitempty"`
}

// EventBus 事件广播：单调 seq + 订阅者。
//
// 广播永不阻塞：慢客户端丢弃的是它自己的副本，不是队列。丢弃计数暴露给
// 指标 player_events_dropped_total，用于发现"某个客户端卡住了"。
type EventBus struct {
	seq       atomic.Uint64
	dropped   atomic.Uint64
	mu        sync.RWMutex
	subs      map[int64]chan Event
	nextSubID int64
	now       func() time.Time
}

// NewEventBus 创建事件总线。
func NewEventBus() *EventBus {
	return &EventBus{subs: make(map[int64]chan Event), now: time.Now}
}

// SetClock 注入时钟（测试用）。
func (b *EventBus) SetClock(now func() time.Time) {
	b.mu.Lock()
	b.now = now
	b.mu.Unlock()
}

// Subscribe 订阅事件，返回通道与取消函数。缓冲区 64 条，满了就丢。
func (b *EventBus) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextSubID++
	id := b.nextSubID
	ch := make(chan Event, 64)
	b.subs[id] = ch
	return ch, func() {
		b.mu.Lock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
		b.mu.Unlock()
	}
}

// Publish 分配 seq 并广播。返回实际发出的事件（seq 已填）。
func (b *EventBus) Publish(e Event) Event {
	e.Seq = b.seq.Add(1)
	b.mu.Lock()
	if b.now != nil {
		e.TimeMs = b.now().UnixMilli()
	}
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default:
			b.dropped.Add(1)
		}
	}
	b.mu.Unlock()
	return e
}

// CurrentSeq 当前 seq（客户端重连时用来判断是否需要全量对齐）。
func (b *EventBus) CurrentSeq() uint64 { return b.seq.Load() }

// Dropped 被丢弃的事件数。
func (b *EventBus) Dropped() uint64 { return b.dropped.Load() }

// EventJSON 编码为一行 JSON（WS 文本帧）。
func (e Event) EventJSON() ([]byte, error) {
	data, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("marshal event: %w", err)
	}
	return data, nil
}

// DecodeEvent 反序列化（客户端 SDK 与测试共用）。
func DecodeEvent(data []byte) (Event, error) {
	var e Event
	if err := json.Unmarshal(data, &e); err != nil {
		return Event{}, fmt.Errorf("unmarshal event: %w", err)
	}
	return e, nil
}

// SeqFilter 按 seq 去重：小于等于已见 seq 的消息一律丢弃。
type SeqFilter struct {
	mu   sync.Mutex
	last uint64
}

// Accept 判断是否应当处理该 seq。
func (f *SeqFilter) Accept(seq uint64) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if seq <= f.last {
		return false
	}
	f.last = seq
	return true
}

// Last 已见的最大 seq。
func (f *SeqFilter) Last() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

// 编译期断言：sink 包的事件类型不被 player 直接复用，避免两个包语义漂移。
var _ = sink.Event{}
