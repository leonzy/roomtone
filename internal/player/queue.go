package player

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
)

// RepeatMode 循环模式。
type RepeatMode string

const (
	RepeatOff RepeatMode = "off"
	RepeatAll RepeatMode = "all"
	RepeatOne RepeatMode = "one"
)

// ErrQueueEmpty 队列为空时取当前项。
var ErrQueueEmpty = errors.New("queue is empty")

// ErrQueueIndexOutOfRange 越界。
var ErrQueueIndexOutOfRange = errors.New("queue index out of range")

// Queue 播放队列，语义对齐 LMS Playlist.pm。
//
// 关键不变量：
//   - items 是**规范顺序**（用户看到的顺序），shuffle 只影响播放顺序；
//   - current 始终是 items 的下标，切歌时由 order 反查；
//   - 关闭 shuffle 后立即回到规范顺序的当前位置，不会"从随机序中间继续"。
//
// 预加载判定用 Remaining()（剩余 < 30s 触发下一首解码），
// 它是纯函数，不依赖时钟，便于表驱动测试。
type Queue struct {
	mu      sync.Mutex
	items   []int64
	current int // -1 表示未选中
	repeat  RepeatMode
	shuffle bool
	order   []int // 播放顺序（items 下标的排列）
	pos     int   // 当前项在 order 中的位置
	rnd     *rand.Rand
}

// NewQueue 创建空队列。
func NewQueue() *Queue {
	return &Queue{current: -1, repeat: RepeatOff, rnd: rand.New(rand.NewSource(1))}
}

// SetRand 注入确定性随机源（测试用）。
func (q *Queue) SetRand(r *rand.Rand) {
	q.mu.Lock()
	q.rnd = r
	q.mu.Unlock()
}

// Items 返回规范顺序的副本。
func (q *Queue) Items() []int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]int64, len(q.items))
	copy(out, q.items)
	return out
}

// Len 队列长度。
func (q *Queue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// Current 当前下标（-1 表示无）。
func (q *Queue) Current() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.current
}

// CurrentSong 当前歌曲 id。
func (q *Queue) CurrentSong() (int64, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.current < 0 || q.current >= len(q.items) {
		return 0, ErrQueueEmpty
	}
	return q.items[q.current], nil
}

// Repeat 当前循环模式。
func (q *Queue) Repeat() RepeatMode {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.repeat
}

// SetRepeat 设置循环模式。
func (q *Queue) SetRepeat(m RepeatMode) {
	q.mu.Lock()
	q.repeat = m
	q.mu.Unlock()
}

// Shuffle 是否随机。
func (q *Queue) Shuffle() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.shuffle
}

// SetShuffle 开关随机。开启时把当前项放在随机序首位，避免"一开随机就跳走"。
func (q *Queue) SetShuffle(on bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if on == q.shuffle {
		return
	}
	q.shuffle = on
	if !on {
		// 回到规范顺序：order 重建为 0..n-1，pos 跟随 current。
		q.order = nil
		q.pos = 0
		if q.current >= 0 {
			q.pos = q.current
		}
		return
	}
	n := len(q.items)
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	rnd := q.rnd
	if rnd == nil {
		rnd = rand.New(rand.NewSource(1))
	}
	rnd.Shuffle(n, func(i, j int) { order[i], order[j] = order[j], order[i] })
	// 当前项提到最前。
	if q.current >= 0 && q.current < n {
		for i, idx := range order {
			if idx == q.current {
				order[0], order[i] = order[i], order[0]
				break
			}
		}
		q.pos = 0
	}
	q.order = order
}

// Replace 用给定歌曲覆盖队列，并把当前项设为 first。
func (q *Queue) Replace(songIDs []int64, first int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append([]int64(nil), songIDs...)
	if len(q.items) == 0 {
		q.current = -1
	} else if first < 0 || first >= len(q.items) {
		q.current = 0
	} else {
		q.current = first
	}
	q.rebuildOrderLocked()
}

// Append 追加歌曲，返回新长度。
func (q *Queue) Append(songIDs ...int64) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	had := len(q.items)
	q.items = append(q.items, songIDs...)
	if q.current < 0 && len(q.items) > 0 {
		q.current = 0
	}
	q.rebuildOrderLocked()
	_ = had
	return len(q.items)
}

// InsertNext 插播：插到当前项之后（规范顺序上），并把它作为下一首。
func (q *Queue) InsertNext(songID int64) int {
	return q.InsertNextMany(songID)
}

// InsertNextMany 批量插播：按传入顺序插到当前项之后，并保证它们
// 在随机播放顺序里也紧跟当前项（否则开了 shuffle 后“下一首”会失效）。
// 返回第一首插入项在规范顺序中的下标。
func (q *Queue) InsertNextMany(songIDs ...int64) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(songIDs) == 0 {
		return q.current
	}
	if len(q.items) == 0 {
		q.items = append([]int64(nil), songIDs...)
		q.current = 0
		q.rebuildOrderLocked()
		return 0
	}
	at := q.current + 1
	items := make([]int64, 0, len(q.items)+len(songIDs))
	items = append(items, q.items[:at]...)
	items = append(items, songIDs...)
	items = append(items, q.items[at:]...)
	q.items = items
	priority := make([]int, len(songIDs))
	for i := range priority {
		priority[i] = at + i
	}
	q.rebuildOrderWithPriorityLocked(priority)
	return at
}

// Remove 删除指定下标。删的是当前项时，新当前项取同位置的下一首。
func (q *Queue) Remove(index int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if index < 0 || index >= len(q.items) {
		return ErrQueueIndexOutOfRange
	}
	q.items = append(q.items[:index], q.items[index+1:]...)
	if len(q.items) == 0 {
		q.current = -1
	} else if index < q.current {
		q.current--
	} else if index == q.current && q.current >= len(q.items) {
		q.current = len(q.items) - 1
	}
	q.rebuildOrderLocked()
	return nil
}

// Move 把 from 位置的歌曲移到 to 位置（重排）。当前项跟着歌曲走。
func (q *Queue) Move(from, to int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.items)
	if from < 0 || from >= n || to < 0 || to >= n {
		return ErrQueueIndexOutOfRange
	}
	if from == to {
		return nil
	}
	cur := q.items[q.current]
	item := q.items[from]
	q.items = append(q.items[:from], q.items[from+1:]...)
	q.items = append(q.items[:to], append([]int64{item}, q.items[to:]...)...)
	for i, id := range q.items {
		if id == cur && i >= 0 {
			// 用 id 反查可能撞重复歌曲，这里以"移动前当前项的内容"为准已足够，
			// 重复 id 时取第一个，行为稳定。
			q.current = i
			break
		}
	}
	q.rebuildOrderLocked()
	return nil
}

// Jump 跳到规范顺序的指定下标。
func (q *Queue) Jump(index int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if index < 0 || index >= len(q.items) {
		return ErrQueueIndexOutOfRange
	}
	q.current = index
	if q.shuffle {
		for i, idx := range q.order {
			if idx == index {
				q.pos = i
				break
			}
		}
	}
	return nil
}

// Next 前进一首。
//
// auto=true 表示"当前曲目自然播完"，此时 repeat=one 原地重播；
// auto=false 表示用户主动点下一首，**即使 repeat=one 也要推进**
// （否则用户会觉得按钮坏了）。
// 返回是否成功推进；false 表示队列到头且 repeat=off，调用方应停下。
func (q *Queue) Next(auto bool) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.items)
	if n == 0 {
		return false
	}
	if auto && q.repeat == RepeatOne {
		return true // 位置不变，重播当前
	}
	if q.shuffle && len(q.order) == n {
		if q.pos+1 < len(q.order) {
			q.pos++
			q.current = q.order[q.pos]
			return true
		}
		if q.repeat == RepeatAll {
			q.pos = 0
			q.current = q.order[0]
			return true
		}
		return false
	}
	if q.current+1 < n {
		q.current++
		return true
	}
	if q.repeat == RepeatAll {
		q.current = 0
		return true
	}
	return false
}

// Prev 后退一首。已在第一首且 repeat=off 时返回 false。
// PeekNext 看一眼下一首会放什么，不移动游标（预取用）。
func (q *Queue) PeekNext() (int64, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.items)
	if n == 0 {
		return 0, false
	}
	if q.shuffle && len(q.order) == n {
		if q.pos+1 < len(q.order) {
			return q.items[q.order[q.pos+1]], true
		}
		if q.repeat == RepeatAll {
			return q.items[q.order[0]], true
		}
		return 0, false
	}
	if q.current+1 < n {
		return q.items[q.current+1], true
	}
	if q.repeat == RepeatAll {
		return q.items[0], true
	}
	return 0, false
}

func (q *Queue) Prev() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := len(q.items)
	if n == 0 {
		return false
	}
	if q.shuffle && len(q.order) == n {
		if q.pos > 0 {
			q.pos--
			q.current = q.order[q.pos]
			return true
		}
		if q.repeat == RepeatAll {
			q.pos = len(q.order) - 1
			q.current = q.order[q.pos]
			return true
		}
		return false
	}
	if q.current > 0 {
		q.current--
		return true
	}
	if q.repeat == RepeatAll {
		q.current = n - 1
		return true
	}
	return false
}

// Remaining 当前曲目剩余时长与是否应预加载下一首。
// 剩余 < preloadWindow 且队列后面还有内容时 shouldPreload=true。
const preloadWindow = 30 * 1e9 // 30s，单位 ns，避免调用方再做换算

// Remaining 返回剩余时长；duration<=0（未知）时返回 0 且不触发预加载。
func (q *Queue) Remaining(position, duration float64) (remaining float64, shouldPreload bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if duration <= 0 {
		return 0, false
	}
	remaining = duration - position
	if remaining < 0 {
		remaining = 0
	}
	if remaining > float64(preloadWindow)/1e9 {
		return remaining, false
	}
	n := len(q.items)
	if n == 0 {
		return remaining, false
	}
	hasNext := q.current+1 < n || q.repeat == RepeatAll
	return remaining, hasNext
}

// Snapshot 队列快照（落库与 API 共用）。
type QueueSnapshot struct {
	Items   []int64    `json:"items"`
	Current int        `json:"current"`
	Repeat  RepeatMode `json:"repeat"`
	Shuffle bool       `json:"shuffle"`
}

// Snapshot 取快照。
func (q *Queue) Snapshot() QueueSnapshot {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := make([]int64, len(q.items))
	copy(items, q.items)
	return QueueSnapshot{Items: items, Current: q.current, Repeat: q.repeat, Shuffle: q.shuffle}
}

// Restore 从快照恢复。
func (q *Queue) Restore(s QueueSnapshot) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append([]int64(nil), s.Items...)
	if len(q.items) == 0 {
		q.current = -1
	} else if s.Current < 0 || s.Current >= len(q.items) {
		q.current = 0
	} else {
		q.current = s.Current
	}
	q.repeat = s.Repeat
	if q.repeat == "" {
		q.repeat = RepeatOff
	}
	q.shuffle = s.Shuffle
	q.rebuildOrderLocked()
}

// rebuildOrderLocked 在结构变化后重建播放顺序，保持当前项位置不变。
func (q *Queue) rebuildOrderLocked() {
	q.rebuildOrderWithPriorityLocked(nil)
}

// rebuildOrderWithPriorityLocked 重建播放顺序；priority 中的下标会紧跟
// 当前项，其余项再随机。用于“下一首播放”在 shuffle 下仍然真的下一首。
func (q *Queue) rebuildOrderWithPriorityLocked(priority []int) {
	n := len(q.items)
	if !q.shuffle || n == 0 {
		q.order = nil
		q.pos = q.current
		return
	}
	cur := q.current
	order := make([]int, 0, n)
	seen := make(map[int]bool, n)
	if cur >= 0 && cur < n {
		order = append(order, cur)
		seen[cur] = true
	}
	for _, idx := range priority {
		if idx >= 0 && idx < n && !seen[idx] {
			order = append(order, idx)
			seen[idx] = true
		}
	}
	rest := make([]int, 0, n-len(order))
	for i := 0; i < n; i++ {
		if !seen[i] {
			rest = append(rest, i)
		}
	}
	rnd := q.rnd
	if rnd == nil {
		rnd = rand.New(rand.NewSource(1))
	}
	rnd.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
	order = append(order, rest...)
	q.order = order
	q.pos = 0
}

// ValidateRepeat 校验循环模式字符串。
func ValidateRepeat(s string) (RepeatMode, error) {
	switch RepeatMode(s) {
	case RepeatOff, RepeatAll, RepeatOne:
		return RepeatMode(s), nil
	case "":
		return RepeatOff, nil
	}
	return "", fmt.Errorf("invalid repeat mode %q", s)
}
