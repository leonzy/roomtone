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
	// Format / SampleRate / BitRate 源文件格式，仅用于 signal_path 诊断。
	// 解码仍由 ffmpeg 按实际文件处理，不参与组内格式协商。
	Format     string
	SampleRate int
	BitRate    int
	// Artist / Album / ArtworkPath 设备屏显元数据（RAOP DMAP / RAAT
	// update_transport）。仅元数据推送用，不参与解码与协商。
	Artist      string
	Album       string
	ArtworkPath string
}

// Decoder 一条连续吐 s16le PCM 的解码流。
type Decoder interface {
	io.ReadCloser
	// Err 在 EOF 之后给出退出原因（ffmpeg 非零退出码 + stderr 尾部）。
	Err() error
}

// DecoderFactory 打开解码流。抽成接口是为了让引擎测试不依赖 ffmpeg。
type DecoderFactory interface {
	Open(ctx context.Context, track Track, startAt time.Duration, format Format) (Decoder, error)
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
	// Metrics 运行指标；nil 时静默（测试与 Bundle 精简路径）。
	Metrics *Metrics
	// ReplayGain 每首歌的增益来源；nil 或模式为 off 时完全不加增益。
	ReplayGain ReplayGainSource
	// Crossfade 交叉淡化时长；返回 0 表示关闭。每次起播读一次，改设置下一曲生效。
	//
	// 与 gapless 互斥（产品决策）：开着 crossfade 时交界处按普通路径重新
	// Open，不再走 GaplessSink.Continue。
	Crossfade func() time.Duration
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
	// sourceFormat 当前曲目的源格式摘要（signal_path 用）；空串表示未知。
	sourceFormat string
	cancel       context.CancelFunc
	running      bool
	release      func()
	underruns    int64
	preload      Decoder
	preloadID    int64
	// sinksOpen 当前 sink 会话已经打开（下一曲可以走 gapless 接续）。
	sinksOpen bool
	lastErr   error
	// curPlan / planBase / cursor 是"播放中加入成员"所需的现场：
	// 新设备要按当前曲目与当前进度 Open，并 Start 到当前时间轴位置。
	curPlan  sink.StreamPlan
	planBase int64
	cursor   int64
	// curTrack 当前曲目的完整解析结果（含 Artist/Album/ArtworkPath），
	// 供 pushMetadata 推送设备屏显。
	curTrack Track
	// controls 控制型 sink（本机客户端自拉流）：服务端不下发 PCM，只发指令。
	//
	// 与 targets 分开存：sink.Set 的元素类型是 PushSink，控制型 sink 进不去。
	// 纯控制型组（组内只有本机）走委派播放，见 runDelegated。
	controls []sink.Sink
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

// Format 当前协商后的输出格式（signal_path 诊断用）。
func (e *Engine) Format() Format {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.format
}

// SourceFormat 当前曲目的源格式摘要；未知返回 "unknown"。
func (e *Engine) SourceFormat() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sourceFormat == "" {
		return "unknown"
	}
	return e.sourceFormat
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

// SetControls 全量替换控制型 sink。
//
// 控制型 sink 不接收 PCM：服务端只下发 load/play/pause/seek 指令，客户端
// 自己拉流解码。它们与 targets 分开管理，因为 sink.Set 只装 PushSink。
func (e *Engine) SetControls(cs []sink.Sink) {
	e.mu.Lock()
	e.controls = append([]sink.Sink(nil), cs...)
	e.mu.Unlock()
}

// Controls 当前控制型 sink 快照（只读用途）。
func (e *Engine) Controls() []sink.Sink {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]sink.Sink(nil), e.controls...)
}

// controlSinks 取控制型 sink 快照；调用方不得持锁。
func (e *Engine) controlSinks() []sink.Sink { return e.Controls() }

// commandControls 把一条指令广播给所有控制型 sink。
//
// 单个成员失败不影响其它成员：客户端断线是常态，重连后管理器会重建 sink，
// 这里只记日志，不把整组拖进错误态。
func (e *Engine) commandControls(op string, fn func(sink.Sink) error) {
	for _, s := range e.controlSinks() {
		if err := fn(s); err != nil {
			e.deps.Logger.Warn("control sink command failed",
				"player_id", s.ID(), "cmd", op, "err", err)
		}
	}
}

// delegatedLocked 判断当前组是否只能走委派播放。
//
// 条件：没有任何推送目标，但至少有一个控制型 sink。此时服务端拿不到任何
// PCM 出口，跑解码环只会得到 distributor: no sinks——而指令其实已经生效，
// 客户端正在正常出声。
func (e *Engine) delegatedLocked() bool {
	if e.targets.Len() > 0 {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.controls) > 0
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

// SetQueue 只替换队列与当前下标，不启动解码。
//
// 客户端是队列的编辑者（选歌、拖拽、删除都在客户端完成），服务端只负责
// 保存"这个区域现在排了什么"。播放仍由客户端驱动，状态靠上报镜像。
func (e *Engine) SetQueue(songIDs []int64, index int) error {
	e.mu.Lock()
	prevID, prevErr := e.queue.CurrentSong()
	e.queue.Replace(songIDs, index)
	nextID, nextErr := e.queue.CurrentSong()
	// 正在解码时当前曲目被换掉（含清空队列）：停掉解码落到 paused，
	// 否则服务端会继续播一首已经不在队列里的歌。
	if e.running && (nextErr != nil || prevErr != nil || prevID != nextID) {
		e.stopLocked()
		if nextErr == nil {
			e.transport.RestorePaused(nextID, 0)
		} else {
			e.transport.Stop()
		}
	} else if !e.running {
		// 没在解码：只把当前曲目换成队列里的那一首，状态机不动。
		if nextErr == nil {
			e.transport.SelectSong(nextID)
		} else {
			e.transport.Stop()
		}
	}
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
	// loading 但没有解码协程 = 死状态（恢复失败、协程异常退出）。
	// 正常 loading 下 Play 是空操作（等缓冲水位），这里必须区分开，
	// 否则一次异常就会让区域永久无法起播。
	st := e.transport.State()
	if st == StateIdle || st == StateError || (st == StateLoading && !e.running) {
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
	// 控制型成员自己拉流，服务端停解码不会让它们停下来，必须显式下指令。
	e.commandControls("pause", func(s sink.Sink) error {
		if p, ok := s.(interface{ Pause() error }); ok {
			return p.Pause()
		}
		return nil
	})
	e.persistState()
	e.emitState()
	return nil
}

// Stop 停止播放并清空当前曲目（队列保留）。
func (e *Engine) Stop() {
	e.mu.Lock()
	e.stopLocked()
	e.transport.Stop()
	e.sourceFormat = ""
	e.mu.Unlock()
	e.commandControls("stop", func(s sink.Sink) error { return s.Stop() })
	e.persistState()
	e.emitState()
}

// Seek 跳转到 position。
func (e *Engine) Seek(ctx context.Context, position time.Duration) error {
	e.mu.Lock()
	// loading 也算“在播”：此时 run 协程正在开解码器，若不重启，
	// stopLocked 杀掉协程后状态机会永远停在 loading（音箱在缓冲期按
	// seek 就会触发）。
	wasPlaying := e.transport.State() == StatePlaying || e.transport.State() == StateBuffering || e.transport.State() == StateLoading
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
	if wasPlaying {
		rate := float64(e.Format().SampleRate)
		e.commandControls("seek", func(s sink.Sink) error {
			return s.Seek(uint64(position.Seconds() * rate))
		})
	}
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
	// 播放中切歌：停掉当前解码环，立即起播下一首（对齐 Prev 的
	// stopLocked→startLocked 模式）。此前缺这一步，startLocked 因
	// running=true 提前返回，队列游标推进后旧解码环继续播完当前曲，
	// EOF 处 queue.Next(true) 再推一格——用户按下一首反而整组停播。
	e.stopLocked()
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
	e.InsertNextMany(ctx, songID)
}

// InsertNextMany 批量插播：按传入顺序插到当前曲之后。
func (e *Engine) InsertNextMany(ctx context.Context, songIDs ...int64) {
	e.queue.InsertNextMany(songIDs...)
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
	if e.eqActiveLocked() {
		reason = "equalizer"
	}
	return e.deps.Budget.Acquire(e.ownerKind+":"+e.ownerID, reason)
}

// eqActiveLocked 组内是否有成员启用了非 flat 的 EQ。
//
// 有 EQ 的组必须走服务端解码（不能直通原文件），槽位耗尽时的提示文案
// 与"同时播放数已满"区分开，用户才知道该关 EQ 还是该停一个组。
func (e *Engine) eqActiveLocked() bool {
	ts, _ := e.targets.Snapshot()
	for _, t := range ts {
		if f, ok := t.EQ.(interface{ Flat() bool }); ok && !f.Flat() {
			return true
		}
	}
	return false
}

// stopLocked 停掉当前解码协程。
// pushMetadata 设备屏显推送循环（A1）：进度 1s 一推，曲目变化时推元数据与封面。
//
// 只对实现对应接口的 sink 生效（RAOP DMAP / RAAT update_transport），
// 其余成员静默跳过；推送失败只记日志，不影响播放主路径。
func (e *Engine) pushMetadata(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	lastSong := int64(-1)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		e.mu.Lock()
		track := e.curTrack
		e.mu.Unlock()
		if track.SongID == 0 {
			continue
		}
		// targets（推帧设备）+ controls（自拉流客户端）里实现对应接口的成员。
		members := make([]sink.Sink, 0)
		targets, _ := e.targets.Snapshot()
		for _, t := range targets {
			members = append(members, t.Sink)
		}
		members = append(members, e.controlSinks()...)

		if track.SongID != lastSong {
			lastSong = track.SongID
			np := sink.NowPlaying{Title: track.Title, Artist: track.Artist, Album: track.Album}
			for _, s := range members {
				if m, ok := s.(sink.MetadataSink); ok {
					if err := m.UpdateMetadata(np); err != nil {
						e.deps.Logger.Warn("metadata push failed", "player_id", s.ID(), "err", err)
					}
				}
				if a, ok := s.(sink.ArtworkSink); ok {
					// 空串表示清除：没有封面也推，设备端据此清屏。
					if err := a.UpdateArtwork(track.ArtworkPath); err != nil {
						e.deps.Logger.Warn("artwork push failed", "player_id", s.ID(), "err", err)
					}
				}
			}
		}
		pos := e.transport.Position()
		for _, s := range members {
			if p, ok := s.(sink.ProgressSink); ok {
				if err := p.UpdateProgress(pos, track.Duration); err != nil {
					e.deps.Logger.Warn("progress push failed", "player_id", s.ID(), "err", err)
				}
			}
		}
	}
}

func (e *Engine) stopLocked() {
	if e.cancel != nil {
		e.cancel()
		e.cancel = nil
	}
	e.running = false
	e.sinksOpen = false
	// Push sinks own an independent transport session. Stopping decoding alone
	// leaves that session open and makes the next Open (resume/seek) fail.
	targets, _ := e.targets.Snapshot()
	for _, target := range targets {
		if err := target.Sink.Stop(); err != nil {
			e.deps.Logger.Warn("stop push sink failed", "player_id", target.PlayerID, "err", err)
		}
	}
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
//
// 播放会话的生命周期由引擎自己持有：调用方多半是 HTTP handler，返回后
// net/http 立刻取消 r.Context()，直接继承会让起播协程第一次循环就退出
// （线上表现为 state=error、error="start ffmpeg: context canceled"）。
// WithoutCancel 只保留 context 里的值（trace 等），取消权交给 stopLocked。
func (e *Engine) startLocked(parent context.Context, position time.Duration) {
	if e.running {
		return
	}
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	e.cancel = cancel
	e.running = true
	e.sourceFormat = ""
	songID, _ := e.queue.CurrentSong()
	e.transport.Load(songID, position)
	// Sample indices begin at the resumed position, not zero. Keep the first
	// resumed sample at now+latency instead of waiting the elapsed track time.
	e.timeline = NewTimeline(e.deps.Clock().Add(-position), e.format.SampleRate, e.deps.Latency)
	go e.run(ctx, position)
	go e.pushMetadata(ctx)
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

	// 纯控制型组（组内只有本机客户端）：服务端没有 PCM 可推，解码环必然
	// 以 ErrNoSinks 失败。这类组走「下发指令 + 跟随上报」的委派模式。
	if e.delegatedLocked() {
		e.runDelegated(ctx, startAt)
		return
	}

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
		e.mu.Lock()
		e.sourceFormat = ""
		e.mu.Unlock()
		track, err := e.resolve(ctx, songID)
		if err != nil {
			e.fail(err)
			return
		}
		e.mu.Lock()
		e.sourceFormat = sourceFormatDetail(track)
		format := e.format
		timeline := e.timeline
		e.mu.Unlock()

		if timeline == nil {
			timeline = NewTimeline(e.deps.Clock(), format.SampleRate, e.deps.Latency)
			e.mu.Lock()
			e.timeline = timeline
			e.mu.Unlock()
		}

		dec, err := e.openDecoder(ctx, track, position, format)
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
		e.curTrack = track
		e.planBase = sampleBase
		e.cursor = sampleBase
		e.mu.Unlock()
		e.mu.Lock()
		resume := e.sinksOpen
		e.mu.Unlock()
		targets, _ := e.targets.Snapshot()
		// crossfade 开启时放弃 gapless 接续（产品决策）：两条路径都要改交界处的
		// 样本，混在一起会让时间轴对不上，只能二选一。
		if resume && canGapless(targets) && e.crossfadeDuration() == 0 {
			// 无缝接续：不重新握手、不重新 Start。RTP 时间戳由帧的
			// SampleIndex 推导，跨曲本来就连着，所以这里只更新曲目信息。
			for _, t := range targets {
				if g, ok := t.Sink.(sink.GaplessSink); ok {
					if err := g.Continue(plan); err != nil {
						e.deps.Logger.Warn("gapless continue failed", "player_id", t.PlayerID, "err", err)
					}
				}
			}
		} else {
			if resume && len(targets) > 0 {
				// 有成员不支持接续（DLNA 这类拉流设备）：退回普通切换，
				// 交界处会有一次 Open/Play，属于已知降级。
				e.deps.Logger.Info("gapless unavailable, falling back to reopen",
					"owner", e.ownerKind+":"+e.ownerID)
			}
			// 三段式起播（B8）：① Open 全部（失败即 fail）→ ② 时钟协调
			//（targets 中 ClockParticipant ≥2 时 master=PlayerID 字典序最小，
			// 其余 SyncToClock 跟随；单参与者跳过）→ ③ Start 全部。
			// 对现有 sink 无行为影响——RAOP 的 Open 重/Start 轻，先全 Open
			// 反而缩小起播散差。
			for _, t := range targets {
				if err := t.Sink.Open(ctx, plan); err != nil {
					e.fail(fmt.Errorf("open sink %s: %w", t.PlayerID, err))
					return
				}
			}
			var clocks []sink.ClockParticipant
			for _, t := range targets {
				if cp, ok := t.Sink.(sink.ClockParticipant); ok {
					clocks = append(clocks, cp)
				}
			}

			if len(clocks) >= 2 {
				// master = PlayerID 字典序最小（稳定选举，无需通信）
				master := clocks[0]
				for _, cp := range clocks[1:] {
					if clockParticipantID(cp) < clockParticipantID(master) {
						master = cp
					}
				}
				mh, mp := master.ClockEndpoint()
				for _, cp := range clocks {
					if cp == master {
						continue
					}
					if err := cp.SyncToClock(mh, mp); err != nil {
						e.deps.Logger.Warn("clock sync failed",
							"owner", e.ownerKind+":"+e.ownerID, "err", err)
					}
				}
			}
			for _, t := range targets {
				if err := t.Sink.Start(timeline.At(uint64(sampleBase)).Add(t.Delay)); err != nil {
					e.fail(fmt.Errorf("start sink %s: %w", t.PlayerID, err))
					return
				}
			}
			e.mu.Lock()
			e.sinksOpen = true
			e.mu.Unlock()
		}
		// 控制型成员（本机客户端自拉流）与推送成员同组时也要收到 load/play，
		// 否则浏览器那一端不会出声。它们不接收 PCM，只按指令自己拉流。
		for _, s := range e.controlSinks() {
			if err := s.Open(ctx, plan); err != nil {
				e.fail(fmt.Errorf("open sink %s: %w", s.ID(), err))
				return
			}
			if err := s.Start(timeline.At(uint64(sampleBase))); err != nil {
				e.fail(fmt.Errorf("start sink %s: %w", s.ID(), err))
				return
			}
		}

		runner := e.deps.Runners(timeline, e.targets, sampleBase, func(idx int64, at time.Time) {
			e.onProgress(idx, at, track.Duration, format.SampleRate)
		})
		// ReplayGain：分发器每曲新建，增益跟着曲目走，不跨曲残留。
		e.applyReplayGain(ctx, runner, track.SongID)
		// 同步校正环：与分发器同生命周期，ctx 结束即退出。
		if cr, ok := runner.(CorrectionRunner); ok {
			loop := &SyncLoop{
				Targets:   e.targets,
				Timeline:  timeline,
				Corrector: cr.Corrector(),
				Clock:     e.deps.Clock,
				OnAdjust: func(a SyncAdjustment) {
					e.deps.Metrics.ObserveDrift(e.ownerKind+":"+e.ownerID, a.Delta)
					e.deps.Logger.Info("sync adjust",
						"owner", e.ownerKind+":"+e.ownerID,
						"player", a.PlayerID, "action", string(a.Action),
						"delta_ms", a.Delta.Milliseconds(), "applied_ms", a.Applied.Milliseconds())
				},
				OnDeviation: func(playerID string, delta time.Duration) {
					e.deps.Metrics.AddResync()
					e.deps.Logger.Warn("sync deviation too large",
						"owner", e.ownerKind+":"+e.ownerID,
						"player", playerID, "delta_ms", delta.Milliseconds())
				},
			}
			go loop.Run(ctx)
		}
		// 交叉淡化：把当前曲尾巴与下一曲开头等功率混合。混合后下一曲的
		// 头部已经被消耗，循环下一轮从 position=fade 起播。
		fade := e.crossfadeDuration()
		var src io.Reader = dec
		var xf *crossfadeReader
		if fade > 0 {
			if nextID, ok := e.queue.PeekNext(); ok && nextID != track.SongID {
				// 帧字节数固定 4（s16le 双声道），与 pipeline 保持一致。
				xf = newCrossfadeReader(dec, crossfadeFrames(fade.Seconds(), format.SampleRate), 4,
					func() (io.ReadCloser, error) {
						return e.openCrossfadeNext(ctx, nextID, format)
					})
				src = xf
			}
		}
		frames, runErr := runner.Run(ctx, src)
		mixed := xf != nil && xf.Mixed()
		if xf != nil {
			_ = xf.Close()
		}
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
		// 做过交叉淡化时，下一曲已经播掉了 fade 秒的头部，从那里接着放。
		position = 0
		if mixed {
			position = fade
		}
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

// delegatedPollInterval 委派播放的进度轮询间隔。
//
// 客户端 500ms 上报一次，这里取一半，位置最多落后半拍；再密就是空转。
const delegatedPollInterval = 250 * time.Millisecond

// runDelegated 控制型 sink 的播放循环。
//
// 与 run 的区别：不打开解码器、不跑分发器。服务端只负责三件事：
//
//  1. 下发 load / play 指令（客户端自己拉流解码）；
//  2. 用客户端上报的进度推进 transport 位置；
//  3. 播完自动切下一首，继续下发指令。
//
// 位置权威在客户端：服务端时间轴只做跟随，不反过来驱动客户端。
func (e *Engine) runDelegated(ctx context.Context, startAt time.Duration) {
	position := startAt
	for {
		if ctx.Err() != nil {
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
		e.sourceFormat = sourceFormatDetail(track)
		plan := sink.StreamPlan{
			Format: sink.Format{
				Codec:      e.format.Codec,
				SampleRate: e.format.SampleRate,
				Channels:   e.format.Channels,
				Bits:       e.format.Bits,
			},
			SongID:   track.SongID,
			Title:    track.Title,
			Position: position,
			Duration: track.Duration,
			Latency:  e.deps.Latency,
		}
		e.curPlan = plan
		e.curTrack = track
		e.planBase = 0
		e.cursor = 0
		e.mu.Unlock()

		// 指令下发：load 带起始位置，play 带绝对起播时刻，客户端据此对齐。
		at := e.deps.Clock()
		controls := e.controlSinks()
		if len(controls) == 0 {
			e.fail(errors.New("区域没有可用设备"))
			return
		}
		for _, s := range controls {
			if err := s.Open(ctx, plan); err != nil {
				e.fail(fmt.Errorf("open sink %s: %w", s.ID(), err))
				return
			}
			if err := s.Start(at); err != nil {
				e.fail(fmt.Errorf("start sink %s: %w", s.ID(), err))
				return
			}
		}

		e.mu.Lock()
		e.sinksOpen = true
		_ = e.transport.Load(track.SongID, position)
		_ = e.transport.MarkBuffering()
		_ = e.transport.Play()
		e.mu.Unlock()
		e.emitState()

		if !e.waitDelegated(ctx, track.Duration) {
			return
		}

		position = 0
		e.mu.Lock()
		advanced := e.queue.Next(true)
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

// waitDelegated 跟随客户端上报推进位置，直到曲目播完或 ctx 取消。
//
// 返回 true 表示自然播完，false 表示被取消（暂停 / 停止 / seek 重建）。
// 客户端不上报时退化为服务端时钟推进，不会永久卡在 0。
func (e *Engine) waitDelegated(ctx context.Context, duration time.Duration) bool {
	ticker := time.NewTicker(delegatedPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
		if pos, ok := e.reportedPosition(); ok {
			e.mu.Lock()
			e.transport.Seek(pos)
			e.cursor = int64(pos.Seconds() * float64(e.format.SampleRate))
			e.mu.Unlock()
		}
		e.mu.Lock()
		pos := e.transport.Position()
		e.mu.Unlock()
		if duration > 0 && pos >= duration {
			return true
		}
	}
}

// reportedPosition 取控制型 sink 上报的播放位置。
//
// 多成员时取最靠前的那个：组内以先到的为准，落后的由客户端自己追。
func (e *Engine) reportedPosition() (time.Duration, bool) {
	var best time.Duration
	found := false
	for _, s := range e.controlSinks() {
		r, ok := s.(sink.PositionReporter)
		if !ok {
			continue
		}
		pos, ok := r.PlaybackPosition()
		if !ok {
			continue
		}
		if !found || pos > best {
			best = pos
			found = true
		}
	}
	return best, found
}

// MirrorReport 把客户端本地播放的状态镜像到服务端。
//
// 用户直接点客户端播放条时，服务端既没下发指令也没有解码协程，组状态会一直
// 停在 idle——UI 上就是「明明在响却显示空闲」。这里在引擎未运行时跟随上报，
// 让区域状态与真实出声一致。
//
// 引擎自己在驱动（running）时一律忽略：那时服务端才是权威，上报只用于位置校正。
func (e *Engine) MirrorReport(songID int64, state string, position time.Duration) {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return
	}
	prev := e.transport.State()
	switch state {
	case "playing":
		e.transport.Mirror(songID, position, true)
	case "paused":
		e.transport.Mirror(songID, position, false)
	case "idle":
		e.transport.Stop()
	default:
		e.mu.Unlock()
		return
	}
	changed := e.transport.State() != prev
	e.mu.Unlock()
	if changed {
		e.emitState()
	}
}

// DefaultFrameSamples 与 pipeline 保持一致：20ms @ 44.1kHz。
const DefaultFrameSamples = 882

// crossfadeDuration 读当前交叉淡化时长；未配置或关闭时返回 0。
func (e *Engine) crossfadeDuration() time.Duration {
	if e.deps.Crossfade == nil {
		return 0
	}
	return e.deps.Crossfade()
}

// openCrossfadeNext 打开交叉淡化用的下一曲解码器。
//
// 优先吃掉预取好的那一份（3s 预取窗口已经把它备好），没有就现开：
// 这一步发生在当前曲 EOF 处，分发器还有 2s 缓冲兜着，几十毫秒的开销
// 不会传到设备侧。返回的 decoder 由 crossfadeReader 负责 Close。
func (e *Engine) openCrossfadeNext(ctx context.Context, songID int64, format Format) (io.ReadCloser, error) {
	e.mu.Lock()
	if e.preload != nil && e.preloadID == songID {
		d := e.preload
		e.preload = nil
		e.preloadID = 0
		e.mu.Unlock()
		return d, nil
	}
	e.mu.Unlock()
	track, err := e.resolve(ctx, songID)
	if err != nil {
		return nil, err
	}
	if e.deps.ReplayGain != nil {
		_, _ = e.deps.ReplayGain.GainDB(ctx, track.SongID)
	}
	return e.deps.Decoder.Open(ctx, track, 0, format)
}

// applyReplayGain 给本曲设置线性增益。
//
// 模式为 off / 没有标签 / 分发器不支持时什么都不做，行为与没有这个功能时一致。
func (e *Engine) applyReplayGain(ctx context.Context, runner FrameRunner, songID int64) {
	if e.deps.ReplayGain == nil || runner == nil {
		return
	}
	db, ok := e.deps.ReplayGain.GainDB(ctx, songID)
	if !ok || db == 0 {
		return
	}
	setter, ok := runner.(gainSetter)
	if !ok {
		return
	}
	setter.SetGain(GainFactor(db))
}

func (e *Engine) resolve(ctx context.Context, songID int64) (Track, error) {
	if e.deps.Resolver == nil {
		return Track{SongID: songID}, nil
	}
	return e.deps.Resolver.ResolveTrack(ctx, songID)
}

// onProgress 每帧回调：更新传输水位与位置，跨过水位线时发一次 state 事件。
func (e *Engine) onProgress(sampleIndex int64, playAt time.Time, duration time.Duration, rate int) {
	e.maybePrefetch(sampleIndex, duration, rate)
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
	//
	// 只有确实存过"正在播/暂停"的状态才恢复。Stop 之后落库的是
	// song_id=0 / state=idle，照搬会把 transport 推进 loading 却没有解码协程，
	// 而 Play 在 loading 下是空操作——区域永久卡死，表现为"点了播放没反应、
	// 区域一直空闲"。
	if st.SongID == 0 || st.State == "" || st.State == string(StateIdle) {
		return nil
	}
	e.transport.RestorePaused(songIDs[idx], time.Duration(st.Position*float64(time.Second)))
	return nil
}

// ErrNoState 没有可恢复的状态。
var ErrNoState = errors.New("no saved playback state")

// clockParticipantID 取 ClockParticipant 的 sink ID（master 选举用）。
func clockParticipantID(cp sink.ClockParticipant) string {
	if s, ok := cp.(sink.Sink); ok {
		return s.ID()
	}
	return ""
}
