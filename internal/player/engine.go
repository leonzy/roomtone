package player

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"songloft/internal/player/sink"
)

// Track 解码所需的曲目信息。
type Track struct {
	SongID   int64
	Title    string
	Path     string
	Duration time.Duration
	// RemoteURL 远程歌曲直接交给 ffmpeg，不落盘。
	RemoteURL string
}

// Decoder 一条连续吐 s16le PCM 的解码流。
type Decoder interface {
	io.ReadCloser
	// Err 在 EOF 之后给出退出原因（ffmpeg 非零退出码 + stderr 尾部）。
	Err() error
}

// DecoderFactory 打开解码流。抽成接口是为了让引擎测试不依赖 ffmpeg。
type DecoderFactory interface {
	Open(ctx context.Context, track Track, startAt time.Duration, format Format, eq EQProfile) (Decoder, error)
}

// FrameRunner 把一条 PCM 流按统一时间轴切帧推给 sinks。
// 由 pipeline.Distributor 实现，注入进来避免 player → pipeline 的循环依赖。
type FrameRunner interface {
	Run(ctx context.Context, r io.Reader) (int, error)
}

// RunnerFactory 为一次播放创建分发器。
//
// targets 是**活的**集合指针：分发器每帧读快照，因此播放中增删成员立即生效，
// 不需要重启解码，也不用等下一曲。
type RunnerFactory func(timeline *Timeline, targets *sink.Set, startSample int64, onProgress func(int64, time.Time)) FrameRunner

// TrackResolver 把 song_id 解析成可解码的曲目。
type TrackResolver interface {
	ResolveTrack(ctx context.Context, songID int64) (Track, error)
}

// StateStore 队列与状态快照的持久化入口（由 database.PlaybackRepository 适配）。
type StateStore interface {
	SaveQueue(ctx context.Context, ownerKind, ownerID string, songIDs []int64) error
	LoadQueue(ctx context.Context, ownerKind, ownerID string) ([]int64, error)
	SaveState(ctx context.Context, ownerKind, ownerID string, s StateSnapshot) error
	LoadState(ctx context.Context, ownerKind, ownerID string) (StateSnapshot, error)
}

// StateSnapshot 断点续播所需的全部状态。
type StateSnapshot struct {
	SongID     int64
	Position   float64
	State      string
	RepeatMode string
	Shuffle    bool
}

// CorrectionRunner 由分发器实现：把自身暴露成同步校正的执行者。
//
// 引擎只管起校正环，不关心校正怎么落地（丢帧 / 插静音都在分发器里）。
type CorrectionRunner interface {
	Corrector() Corrector
}

// EngineDeps 引擎依赖。
type EngineDeps struct {
	Decoder  DecoderFactory
	Runners  RunnerFactory
	Resolver TrackResolver
	Budget   *Budget
	Bus      *EventBus
	Store    StateStore
	Logger   *slog.Logger
	Clock    func() time.Time
	// Latency 起播提前量，默认 2s。
	Latency time.Duration
}

// Engine 一个 owner（组或派对）的播放引擎：队列 + 传输状态机 + 解码分发。
//
// 单机也走这里——单机就是一个只含自己的组。派对与组的差别只在 ownerKind，
// 队列与状态机代码完全复用。
type Engine struct {
	ownerKind string
	ownerID   string

	deps EngineDeps

	mu        sync.Mutex
	queue     *Queue
	transport *Transport
	timeline  *Timeline
	targets   *sink.Set
	format    Format
	eq        EQProfile
	cancel    context.CancelFunc
	running   bool
	release   func()
	underruns int64
	preload   Decoder
	preloadID int64
	lastErr   error
	// curPlan / planBase / cursor 是"播放中加入成员"所需的现场：
	// 新设备要按当前曲目与当前进度 Open，并 Start 到当前时间轴位置。
	curPlan  sink.StreamPlan
	planBase int64
	cursor   int64
}

// NewEngine 创建引擎。
func NewEngine(ownerKind, ownerID string, deps EngineDeps) *Engine {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Clock == nil {
		deps.Clock = time.Now
	}
	if deps.Latency <= 0 {
		deps.Latency = 2 * time.Second
	}
	return &Engine{
		ownerKind: ownerKind,
		ownerID:   ownerID,
		deps:      deps,
		queue:     NewQueue(),
		transport: NewTransport(),
		targets:   sink.NewSet(),
		format:    Format{Codec: CodecL16, SampleRate: 44100, Channels: 2, Bits: 16},
	}
}

// Queue 队列（指针，调用方读快照即可）。
func (e *Engine) Queue() *Queue { return e.queue }

// Transport 状态机。
func (e *Engine) Transport() *Transport { return e.transport }

// SetFormat 设置协商后的输出格式。
func (e *Engine) SetFormat(f Format) {
	e.mu.Lock()
	e.format = f
	e.mu.Unlock()
}

// SetEQ 设置组 EQ（第一版取主控设备的 EQ）。
func (e *Engine) SetEQ(eq EQProfile) {
	e.mu.Lock()
	e.eq = eq
	e.mu.Unlock()
}

// Targets 当前推送目标集合（只读用途）。
func (e *Engine) Targets() *sink.Set { return e.targets }

// SetTargets 全量替换推送目标。
//
// 未在播放：直接替换，下次开播时统一 Open。
// 正在播放：新增的立刻按当前曲目与进度 Open+Start（下一帧就能出声），
// 移除的立刻停止写；只改了 EQ / 延迟的走参数更新，不重建。
func (e *Engine) SetTargets(ctx context.Context, ts []sink.Target) error {
	e.mu.Lock()
	running := e.running
	e.mu.Unlock()
	if !running {
		e.targets.Store(ts)
		return nil
	}
	want := make(map[string]sink.Target, len(ts))
	for _, t := range ts {
		want[t.PlayerID] = t
	}
	for _, id := range e.targets.IDs() {
		if _, ok := want[id]; !ok {
			e.DetachTarget(id)
		}
	}
	for _, t := range ts {
		if old, ok := e.targets.Get(t.PlayerID); ok {
			old.EQ = t.EQ
			old.Delay = t.Delay
			e.targets.Add(old)
			continue
		}
		if err := e.attachTarget(ctx, t); err != nil {
			return err
		}
	}
	return nil
}

// AttachTarget 播放中加入一台设备（也可在空闲时调用，等下次开播生效）。
func (e *Engine) AttachTarget(ctx context.Context, t sink.Target) error {
	e.mu.Lock()
	running := e.running
	e.mu.Unlock()
	if !running {
		e.targets.Add(t)
		return nil
	}
	return e.attachTarget(ctx, t)
}

// DetachTarget 从分发中移除一台设备。只 Stop，不 Close——sink 实例由管理器缓存复用。
func (e *Engine) DetachTarget(playerID string) {
	t, ok := e.targets.Get(playerID)
	e.targets.Remove(playerID)
	if !ok {
		return
	}
	_ = t.Sink.Stop()
}

// attachTarget 按当前曲目与进度把新设备接进正在播的时间轴。
func (e *Engine) attachTarget(ctx context.Context, t sink.Target) error {
	e.mu.Lock()
	plan := e.curPlan
	cursor, base := e.cursor, e.planBase
	timeline := e.timeline
	rate := e.format.SampleRate
	e.mu.Unlock()
	if plan.SongID == 0 || timeline == nil || rate <= 0 {
		// 还没开播：只登记，开播时统一 Open。
		e.targets.Add(t)
		return nil
	}
	offset := cursor - base
	if offset < 0 {
		offset = 0
	}
	plan.Position += time.Duration(float64(offset) / float64(rate) * float64(time.Second))
	if err := t.Sink.Open(ctx, plan); err != nil {
		return fmt.Errorf("open sink %s: %w", t.PlayerID, err)
	}
	if err := t.Sink.Start(timeline.At(uint64(cursor)).Add(t.Delay)); err != nil {
		return fmt.Errorf("start sink %s: %w", t.PlayerID, err)
	}
	e.targets.Add(t)
	return nil
}

// Status 引擎快照，供 API 与 WS 事件使用。
type Status struct {
	OwnerKind string        `json:"owner_kind"`
	OwnerID   string        `json:"owner_id"`
	SongID    int64         `json:"song_id"`
	Position  float64       `json:"position"`
	State     State         `json:"state"`
	Repeat    RepeatMode    `json:"repeat"`
	Shuffle   bool          `json:"shuffle"`
	Queue     QueueSnapshot `json:"queue"`
	Error     string        `json:"error,omitempty"`
	Underruns int64         `json:"underruns"`
}

// Status 取状态快照。
func (e *Engine) Status() Status {
	q := e.queue.Snapshot()
	return Status{
		OwnerKind: e.ownerKind,
		OwnerID:   e.ownerID,
		SongID:    e.transport.SongID(),
		Position:  e.transport.Position().Seconds(),
		State:     e.transport.State(),
		Repeat:    q.Repeat,
		Shuffle:   q.Shuffle,
		Queue:     q,
		Error:     e.transport.Error(),
		Underruns: e.underruns,
	}
}

// Load 用给定队列开播。songIDs 为规范顺序，index 为起始下标。
func (e *Engine) Load(ctx context.Context, songIDs []int64, index int, position time.Duration) error {
	if len(songIDs) == 0 {
		return ErrQueueEmpty
	}
	if index < 0 || index >= len(songIDs) {
		index = 0
	}
	e.mu.Lock()
	e.queue.Replace(songIDs, index)
	release, err := e.acquireLocked()
	if err != nil {
		e.mu.Unlock()
		return err
	}
	e.release = release
	e.startLocked(ctx, position)
	e.mu.Unlock()
	e.persistQueue()
	e.emitState()
	return nil
}

// Play 从暂停恢复。
func (e *Engine) Play(ctx context.Context) error {
	e.mu.Lock()
	if e.transport.State() == StatePaused {
		// 恢复：从暂停位置重建解码器（ffmpeg 不暂停，直接停掉重开）。
		pos := e.transport.Position()
		if e.running {
			e.stopLocked()
		}
		release, err := e.acquireLocked()
		if err != nil {
			e.mu.Unlock()
			return err
		}
		e.release = release
		e.startLocked(ctx, pos)
		e.mu.Unlock()
		e.emitState()
		return nil
	}
	if e.transport.State() == StateIdle || e.transport.State() == StateError {
		songIDs := e.queue.Items()
		idx := e.queue.Current()
		e.mu.Unlock()
		if len(songIDs) == 0 {
			return ErrQueueEmpty
		}
		return e.Load(ctx, songIDs, idx, 0)
	}
	e.mu.Unlock()
	e.emitState()
	return nil
}

// Pause 暂停。解码器停掉，恢复时按位置重开（比留着进程空转省 CPU）。
func (e *Engine) Pause() error {
	e.mu.Lock()
	if err := e.transport.Pause(); err != nil {
		e.mu.Unlock()
		return err
	}
	e.stopLocked()
	e.mu.Unlock()
	e.persistState()
	e.emitState()
	return nil
}

// Stop 停止播放并清空当前曲目（队列保留）。
func (e *Engine) Stop() {
	e.mu.Lock()
	e.stopLocked()
	e.transport.Stop()
	e.mu.Unlock()
	e.persistState()
	e.emitState()
}

// Seek 跳转到 position。
func (e *Engine) Seek(ctx context.Context, position time.Duration) error {
	e.mu.Lock()
	wasPlaying := e.transport.State() == StatePlaying || e.transport.State() == StateBuffering
	e.transport.Seek(position)
	e.stopLocked()
	if wasPlaying {
		release, err := e.acquireLocked()
		if err != nil {
			e.mu.Unlock()
			return err
		}
		e.release = release
		e.startLocked(ctx, position)
	}
	e.mu.Unlock()
	e.persistState()
	e.emitState()
	return nil
}

// Next 下一首。auto=false 是用户主动切歌。
func (e *Engine) Next(ctx context.Context, auto bool) error {
	e.mu.Lock()
	if !e.queue.Next(auto) {
		e.stopLocked()
		e.transport.Stop()
		e.mu.Unlock()
		e.persistState()
		e.emitState()
		return nil
	}
	release, err := e.acquireLocked()
	if err != nil {
		e.mu.Unlock()
		return err
	}
	e.release = release
	e.startLocked(ctx, 0)
	e.mu.Unlock()
	e.persistQueue()
	e.persistState()
	e.emitState()
	return nil
}

// Prev 上一首。
func (e *Engine) Prev(ctx context.Context) error {
	e.mu.Lock()
	if !e.queue.Prev() {
		e.transport.Seek(0)
		e.mu.Unlock()
		return nil
	}
	e.stopLocked()
	release, err := e.acquireLocked()
	if err != nil {
		e.mu.Unlock()
		return err
	}
	e.release = release
	e.startLocked(ctx, 0)
	e.mu.Unlock()
	e.persistState()
	e.emitState()
	return nil
}

// SetRepeat 设置循环模式。
func (e *Engine) SetRepeat(m RepeatMode) {
	e.queue.SetRepeat(m)
	e.persistState()
	e.emitState()
}

// SetShuffle 开关随机。
func (e *Engine) SetShuffle(on bool) {
	e.queue.SetShuffle(on)
	e.persistState()
	e.emitState()
}

// InsertNext 插播一首。
func (e *Engine) InsertNext(ctx context.Context, songID int64) {
	e.queue.InsertNext(songID)
	e.persistQueue()
	e.emitState()
}

// Enqueue 追加歌曲。
func (e *Engine) Enqueue(songIDs ...int64) {
	e.queue.Append(songIDs...)
	e.persistQueue()
	e.emitState()
}

// RemoveAt 删除队列项。
func (e *Engine) RemoveAt(index int) error {
	if err := e.queue.Remove(index); err != nil {
		return err
	}
	e.persistQueue()
	e.emitState()
	return nil
}

// Move 重排队列。
func (e *Engine) Move(from, to int) error {
	if err := e.queue.Move(from, to); err != nil {
		return err
	}
	e.persistQueue()
	e.emitState()
	return nil
}

// Close 释放解码器与预算槽位。
func (e *Engine) Close() {
	e.mu.Lock()
	e.stopLocked()
	e.mu.Unlock()
}

// acquireLocked 申请解码预算。已在播时是幂等的。
func (e *Engine) acquireLocked() (func(), error) {
	if e.deps.Budget == nil {
		return func() {}, nil
	}
	reason := "play"
	if !e.eq.Flat() {
		reason = "equalizer"
	}
	return e.deps.Budget.Acquire(e.ownerKind+":"+e.ownerID, reason)
}

// stopLocked 停掉当前解码协程。
func (e *Engine) stopLocked() {
	if e.cancel != nil {
		e.cancel()
		e.cancel = nil
	}
	e.running = false
	if e.preload != nil {
		_ = e.preload.Close()
		e.preload = nil
		e.preloadID = 0
	}
	if e.release != nil {
		e.release()
		e.release = nil
	}
}

// startLocked 启动播放协程。
func (e *Engine) startLocked(parent context.Context, position time.Duration) {
	if e.running {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	e.cancel = cancel
	e.running = true
	songID, _ := e.queue.CurrentSong()
	e.transport.Load(songID, position)
	e.timeline = NewTimeline(e.deps.Clock(), e.format.SampleRate, e.deps.Latency)
	go e.run(ctx, position)
}

// run 解码 → 分发 → 自动续播的主循环。
//
// 关键：切歌**不重建 Timeline**，采样序号接着往下走，这是无缝播放的地基。
func (e *Engine) run(ctx context.Context, startAt time.Duration) {
	defer func() {
		e.mu.Lock()
		e.running = false
		e.mu.Unlock()
	}()

	position := startAt
	sampleBase := int64(0)
	if position > 0 {
		sampleBase = int64(position.Seconds() * float64(e.format.SampleRate))
	}
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		songID, err := e.queue.CurrentSong()
		if err != nil {
			return
		}
		track, err := e.resolve(ctx, songID)
		if err != nil {
			e.fail(err)
			return
		}
		e.mu.Lock()
		format := e.format
		eq := e.eq
		timeline := e.timeline
		e.mu.Unlock()

		if timeline == nil {
			timeline = NewTimeline(e.deps.Clock(), format.SampleRate, e.deps.Latency)
			e.mu.Lock()
			e.timeline = timeline
			e.mu.Unlock()
		}

		dec, err := e.deps.Decoder.Open(ctx, track, position, format, eq)
		if err != nil {
			e.fail(err)
			return
		}
		e.mu.Lock()
		e.transport.MarkBuffering()
		e.mu.Unlock()
		e.emitState()

		// 打开 sink：每台设备各自 start 到同一时刻。
		plan := sink.StreamPlan{
			Format:   sink.Format{Codec: format.Codec, SampleRate: format.SampleRate, Channels: format.Channels, Bits: format.Bits},
			SongID:   track.SongID,
			Title:    track.Title,
			Position: position,
			Duration: track.Duration,
			Latency:  e.deps.Latency,
		}
		e.mu.Lock()
		e.curPlan = plan
		e.planBase = sampleBase
		e.cursor = sampleBase
		e.mu.Unlock()
		targets, _ := e.targets.Snapshot()
		for _, t := range targets {
			_ = t.Sink.Open(ctx, plan)
			_ = t.Sink.Start(timeline.At(uint64(sampleBase)).Add(t.Delay))
		}

		runner := e.deps.Runners(timeline, e.targets, sampleBase, func(idx int64, at time.Time) {
			e.onProgress(idx, at, track.Duration, format.SampleRate)
		})
		// 同步校正环：与分发器同生命周期，ctx 结束即退出。
		if cr, ok := runner.(CorrectionRunner); ok {
			loop := &SyncLoop{
				Targets:   e.targets,
				Timeline:  timeline,
				Corrector: cr.Corrector(),
				Clock:     e.deps.Clock,
				OnAdjust: func(a SyncAdjustment) {
					e.deps.Logger.Info("sync adjust",
						"owner", e.ownerKind+":"+e.ownerID,
						"player", a.PlayerID, "action", string(a.Action),
						"delta_ms", a.Delta.Milliseconds(), "applied_ms", a.Applied.Milliseconds())
				},
				OnDeviation: func(playerID string, delta time.Duration) {
					e.deps.Logger.Warn("sync deviation too large",
						"owner", e.ownerKind+":"+e.ownerID,
						"player", playerID, "delta_ms", delta.Milliseconds())
				},
			}
			go loop.Run(ctx)
		}
		frames, runErr := runner.Run(ctx, dec)
		_ = dec.Close()

		e.mu.Lock()
		sampleBase = int64(frames)*DefaultFrameSamples + sampleBase
		state := e.transport.State()
		e.mu.Unlock()

		if ctx.Err() != nil {
			return
		}
		if runErr != nil && !errors.Is(runErr, io.EOF) {
			e.fail(fmt.Errorf("decode song %d: %w", track.SongID, runErr))
			return
		}
		if derr := dec.Err(); derr != nil {
			e.fail(derr)
			return
		}
		_ = state

		// 自然播完：auto=true 时 repeat=one 原地重播。
		position = 0
		e.mu.Lock()
		advanced := e.queue.Next(true)
		if advanced && e.queue.Repeat() == RepeatOne {
			// 位置不变，重播同一首
		}
		e.mu.Unlock()
		if !advanced {
			e.mu.Lock()
			e.transport.Stop()
			e.mu.Unlock()
			e.persistState()
			e.emitState()
			return
		}
		e.persistState()
		e.emitState()
	}
}

// DefaultFrameSamples 与 pipeline 保持一致：20ms @ 44.1kHz。
const DefaultFrameSamples = 882

func (e *Engine) resolve(ctx context.Context, songID int64) (Track, error) {
	if e.deps.Resolver == nil {
		return Track{SongID: songID}, nil
	}
	return e.deps.Resolver.ResolveTrack(ctx, songID)
}

// onProgress 每帧回调：更新传输水位与位置，跨过水位线时发一次 state 事件。
func (e *Engine) onProgress(sampleIndex int64, playAt time.Time, duration time.Duration, rate int) {
	e.mu.Lock()
	e.cursor = sampleIndex
	if e.transport.State() == StateBuffering {
		// 水位 = 已推送但尚未播放的时长。
		ahead := playAt.Sub(e.deps.Clock())
		if ahead < 0 {
			ahead = 0
		}
		if e.transport.ReportWatermark(ahead) {
			e.mu.Unlock()
			e.emitState()
			return
		}
	}
	e.mu.Unlock()
}

func (e *Engine) fail(err error) {
	e.lastErr = err
	e.transport.Fail(err)
	if e.deps.Bus != nil {
		e.deps.Bus.Publish(Event{
			Type:    EventError,
			GroupID: e.groupID(),
			Code:    "playback_failed",
			Message: err.Error(),
		})
	}
	e.emitState()
}

// groupID 派对的事件按主控组归属，前端据此定位。
func (e *Engine) groupID() string {
	if e.ownerKind == "group" {
		return e.ownerID
	}
	return ""
}

// emitState 发 state 事件。
func (e *Engine) emitState() {
	if e.deps.Bus == nil {
		return
	}
	st := e.Status()
	e.deps.Bus.Publish(Event{
		Type:     EventState,
		GroupID:  e.groupID(),
		SongID:   st.SongID,
		Position: int64(st.Position * 1000),
		Status:   string(st.State),
	})
}

// persistQueue 落库队列。用独立 flushCtx：主循环取消不能影响落库。
func (e *Engine) persistQueue() {
	if e.deps.Store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	if err := e.deps.Store.SaveQueue(ctx, e.ownerKind, e.ownerID, e.queue.Items()); err != nil && e.deps.Logger != nil {
		e.deps.Logger.Warn("save playback queue failed", "owner", e.ownerID, "err", err)
	}
}

// persistState 落库播放状态。写库失败只记日志，不中断播放。
func (e *Engine) persistState() {
	if e.deps.Store == nil {
		return
	}
	st := e.Status()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	err := e.deps.Store.SaveState(ctx, e.ownerKind, e.ownerID, StateSnapshot{
		SongID:     st.SongID,
		Position:   st.Position,
		State:      string(st.State),
		RepeatMode: string(st.Repeat),
		Shuffle:    st.Shuffle,
	})
	if err != nil && e.deps.Logger != nil {
		e.deps.Logger.Warn("save playback state failed", "owner", e.ownerID, "err", err)
	}
}

// Restore 从库恢复队列与位置（重启后调用）。
func (e *Engine) Restore(ctx context.Context) error {
	if e.deps.Store == nil {
		return nil
	}
	songIDs, err := e.deps.Store.LoadQueue(ctx, e.ownerKind, e.ownerID)
	if err != nil {
		return err
	}
	if len(songIDs) == 0 {
		return nil
	}
	st, err := e.deps.Store.LoadState(ctx, e.ownerKind, e.ownerID)
	if err != nil && !errors.Is(err, ErrNoState) {
		return err
	}
	idx := 0
	for i, id := range songIDs {
		if id == st.SongID {
			idx = i
			break
		}
	}
	e.queue.Replace(songIDs, idx)
	e.queue.SetRepeat(RepeatMode(st.RepeatMode))
	e.queue.SetShuffle(st.Shuffle)
	// 恢复后停在暂停态：不自动出声，用户点播放再继续。
	e.transport.Load(st.SongID, time.Duration(st.Position*float64(time.Second)))
	_ = e.transport.Pause()
	return nil
}

// ErrNoState 没有可恢复的状态。
var ErrNoState = errors.New("no saved playback state")
