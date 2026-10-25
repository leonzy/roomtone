package player

import (
	"sync"
	"sync/atomic"
	"time"
)

// Metrics 播放子系统的运行指标（B8-03）。
//
// 只做"累加"与"最近值"，不做聚合窗口：指标要能在真机上用 curl 一眼看出
// 是"解码跟不上"（underruns 涨）还是"网络堵了"（dropped_frames 涨），
// 所以每个计数器都对应一个明确的物理事件。
//
// 计数器用原子操作，读快照时不加全局锁——播放线程（分发器）只写不读，
// 绝不能因为有人在看指标而卡住音频。
type Metrics struct {
	underruns     atomic.Uint64 // 欠载补静音的帧数（解码或磁盘跟不上）
	droppedFrames atomic.Uint64 // 分发队列满丢掉的帧数（某一路写得太慢）
	resyncs       atomic.Uint64 // 偏差超限被迫重新挂载的次数

	driftMu sync.Mutex
	drift   map[string]int64 // owner("group:x"/"party:y") -> 最近一次校正的绝对偏差(ms)
}

// NewMetrics 创建指标集合。
func NewMetrics() *Metrics {
	return &Metrics{drift: make(map[string]int64)}
}

// AddUnderrun 记一次欠载（frames 为本次补的静音帧数）。
func (m *Metrics) AddUnderrun(frames int) {
	if m == nil || frames <= 0 {
		return
	}
	m.underruns.Add(uint64(frames))
}

// AddDroppedFrames 记被丢弃的帧数。
func (m *Metrics) AddDroppedFrames(n int64) {
	if m == nil || n <= 0 {
		return
	}
	m.droppedFrames.Add(uint64(n))
}

// AddResync 记一次"偏差超限重新挂载"。
func (m *Metrics) AddResync() {
	if m == nil {
		return
	}
	m.resyncs.Add(1)
}

// ObserveDrift 记录某个 owner 的最近偏差（取绝对值，正负都算偏差大小）。
func (m *Metrics) ObserveDrift(owner string, d time.Duration) {
	if m == nil {
		return
	}
	ms := d.Milliseconds()
	if ms < 0 {
		ms = -ms
	}
	m.driftMu.Lock()
	m.drift[owner] = ms
	m.driftMu.Unlock()
}

// DriftMs 取某个 owner 的最近偏差（毫秒）。
func (m *Metrics) DriftMs(owner string) int64 {
	if m == nil {
		return 0
	}
	m.driftMu.Lock()
	defer m.driftMu.Unlock()
	return m.drift[owner]
}

// MetricsSnapshot GET /playback/metrics 的返回体。
//
// 字段名直接用 Prometheus 风格的指标名做参照，前端不需要二次映射；
// 但这是**快照接口**，不是 Prometheus 端点——本项目没有引入指标库，
// 也不打算为了 5 个计数器引入一条采集链路。
type MetricsSnapshot struct {
	DecodeSlotsUsed    int    `json:"decode_slots_used"`
	DecodeSlotsMax     int    `json:"decode_slots_max"`
	UnderrunsTotal     uint64 `json:"player_underruns_total"`
	DroppedFramesTotal uint64 `json:"player_sink_dropped_frames_total"`
	ResyncsTotal       uint64 `json:"player_resyncs_total"`
	EventsDroppedTotal uint64 `json:"player_events_dropped_total"`
	// DriftMs 所有 owner 里最近一次校正偏差的最大值（前端用来判断"组还稳不稳"）。
	DriftMs int64 `json:"player_drift_ms"`
	// Groups / Players 当前规模，便于对照"是不是只有一台设备在响"。
	Groups  int `json:"groups"`
	Players int `json:"players"`
	// Owners 每个在播 owner 的明细（组或派对）。
	Owners []OwnerMetrics `json:"owners"`
}

// OwnerMetrics 一个在播 owner（组 / 派对）的指标明细。
type OwnerMetrics struct {
	Owner    string  `json:"owner"`
	GroupID  string  `json:"group_id,omitempty"`
	PartyID  string  `json:"party_id,omitempty"`
	State    string  `json:"state"`
	SongID   int64   `json:"song_id,omitempty"`
	Position float64 `json:"position"`
	DriftMs  int64   `json:"drift_ms"`
	Targets  int     `json:"targets"`
	// Buffering 长时间停在缓冲说明下游写不进去，比 underruns 更早暴露问题。
	Buffering bool `json:"buffering"`
}

// Metrics 返回指标集合（宿主把分发器的回调接到这里）。
func (m *Manager) Metrics() *Metrics { return m.metrics }

// MetricsSnapshot 汇总当前指标。
func (m *Manager) MetricsSnapshot() MetricsSnapshot {
	snap := MetricsSnapshot{
		Groups:  len(m.groups),
		Players: len(m.players),
	}
	if m.deps.Budget != nil {
		snap.DecodeSlotsUsed = m.deps.Budget.Used()
		snap.DecodeSlotsMax = m.deps.Budget.Max()
	}
	if m.metrics != nil {
		snap.UnderrunsTotal = m.metrics.underruns.Load()
		snap.DroppedFramesTotal = m.metrics.droppedFrames.Load()
		snap.ResyncsTotal = m.metrics.resyncs.Load()
	}
	if m.deps.Bus != nil {
		snap.EventsDroppedTotal = m.deps.Bus.Dropped()
	}

	m.mu.RLock()
	engines := make([]*Engine, 0, len(m.engines))
	for _, e := range m.engines {
		engines = append(engines, e)
	}
	m.mu.RUnlock()

	for _, e := range engines {
		st := e.Status()
		if st.State == StateIdle && st.SongID == 0 {
			continue
		}
		om := OwnerMetrics{
			Owner:     e.ownerKind + ":" + e.ownerID,
			State:     string(st.State),
			SongID:    st.SongID,
			Position:  st.Position,
			Buffering: st.State == StateBuffering,
		}
		if e.ownerKind == "group" {
			om.GroupID = e.ownerID
		} else {
			om.PartyID = e.ownerID
		}
		if m.metrics != nil {
			om.DriftMs = m.metrics.DriftMs(om.Owner)
		}
		targets, _ := e.targets.Snapshot()
		om.Targets = len(targets)
		if om.DriftMs > snap.DriftMs {
			snap.DriftMs = om.DriftMs
		}
		snap.Owners = append(snap.Owners, om)
	}
	return snap
}
