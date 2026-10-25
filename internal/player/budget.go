package player

import (
	"errors"
	"fmt"
	"sync"
)

// 播放器侧的失败原因。与转码槽位（seekStreamSem）的错误刻意区分：
// 用户看到的提示必须是"同时播放数已达上限"而不是"转码繁忙"。
var (
	// ErrBudgetExhausted 并发解码组数已满。
	ErrBudgetExhausted = errors.New("player decode budget exhausted")
	// ErrBudgetEQCaused 槽位是被 EQ 需求占满的（提示文案不同）。
	ErrBudgetEQCaused = errors.New("player decode budget exhausted by equalizer")
)

// Budget 播放器独立解码预算。
//
// 单位是"并发解码组数"（docs/architecture_playback.md §8.4）：
// 每个正在播放的组占 1，派对里每个组各占 1，EQ 非 flat 的组强制占 1。
//
// 与 services.seekStreamSem **互不抢占**：那是两个独立信号量，
// 各自限流。测试 TestBudgetIndependentFromTranscodeSlots 保证这一点。
type Budget struct {
	mu       sync.Mutex
	max      int
	used     int
	eqUsed   int
	holders  map[string]string // holder id -> reason
	onChange func(used, max int)
}

// NewBudget 创建预算，max<=0 时退回默认 4。
func NewBudget(max int) *Budget {
	if max <= 0 {
		max = DefaultMaxGroups
	}
	return &Budget{max: max, holders: make(map[string]string)}
}

// DefaultMaxGroups 默认并发解码组数。
const DefaultMaxGroups = 4

// SetOnChange 注册用量变化回调（指标 / 前端提示用）。
func (b *Budget) SetOnChange(fn func(used, max int)) {
	b.mu.Lock()
	b.onChange = fn
	b.mu.Unlock()
}

// SetMax 调整上限。调小不踢掉已在播的组，只影响后续申请。
func (b *Budget) SetMax(max int) {
	if max <= 0 {
		max = DefaultMaxGroups
	}
	b.mu.Lock()
	b.max = max
	fn := b.onChange
	used := b.used
	b.mu.Unlock()
	if fn != nil {
		fn(used, max)
	}
}

// Acquire 申请一个解码槽位。holder 唯一标识申请者（组 id），重复申请是幂等的。
// 返回的 release 可重复调用。
func (b *Budget) Acquire(holder string, reason string) (func(), error) {
	b.mu.Lock()
	if _, ok := b.holders[holder]; ok {
		b.mu.Unlock()
		return func() {}, nil
	}
	if b.used >= b.max {
		eqFull := b.eqUsed > 0
		max := b.max
		b.mu.Unlock()
		if eqFull && reason == "equalizer" {
			return func() {}, fmt.Errorf("%w: %d/%d", ErrBudgetEQCaused, b.used, max)
		}
		return func() {}, fmt.Errorf("%w: %d/%d", ErrBudgetExhausted, b.used, max)
	}
	b.used++
	if reason == "equalizer" {
		b.eqUsed++
	}
	b.holders[holder] = reason
	used := b.used
	max := b.max
	fn := b.onChange
	b.mu.Unlock()
	if fn != nil {
		fn(used, max)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			if _, ok := b.holders[holder]; !ok {
				b.mu.Unlock()
				return
			}
			delete(b.holders, holder)
			b.used--
			if reason == "equalizer" && b.eqUsed > 0 {
				b.eqUsed--
			}
			used := b.used
			max := b.max
			fn := b.onChange
			b.mu.Unlock()
			if fn != nil {
				fn(used, max)
			}
		})
	}, nil
}

// Used 当前占用数。
func (b *Budget) Used() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// Max 当前上限。
func (b *Budget) Max() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.max
}

// Holders 当前持有者快照（诊断用）。
func (b *Budget) Holders() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]string, len(b.holders))
	for k, v := range b.holders {
		out[k] = v
	}
	return out
}
