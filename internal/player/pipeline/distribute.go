package pipeline

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"songloft/internal/player"
	"songloft/internal/player/sink"
)

// Distributor 把一条 PCM 流切成 20ms 帧分发给所有推送目标。
//
// 关键性质（都有测试守着）：
//   - 每帧固定 FrameSamples 个采样，字节数 = FrameSamples × 4（16bit 双声道）；
//   - SampleIndex 连续单调，跨曲目也不归零（无缝续播的地基）；
//   - 欠载补静音：落后于时间轴时插入静音帧把序号追平，**不推迟后续帧**；
//   - 发送节奏按"领先播放时刻不超过 BufferDepth"，不会一口气灌满设备缓冲；
//   - **每一路目标一个独立写协程 + 独立有界队列**：一台设备卡住不拖累其他成员，
//     队列满时丢最旧的帧并计数（宁可让那一路跳一小段，也不能让全组一起卡）；
//   - **目标集合可热替换**：Targets 是 *sink.Set，播放中增删成员下一帧生效。
type Distributor struct {
	Rate         int
	FrameSamples int
	// Targets 推送目标集合（可热替换）。为空返回 ErrNoSinks。
	Targets  *sink.Set
	Timeline *player.Timeline
	// BufferDepth 允许领先播放时刻多少（默认 2s，与 RAOP latency 对齐）。
	BufferDepth time.Duration
	// StartSample 起始采样序号（seek 后接着原时间轴）。
	StartSample int64
	// OnUnderrun 欠载回调，参数是本次补的帧数。
	OnUnderrun func(frames int)
	// OnProgress 每帧回调（用于上报水位 / 位置）。
	OnProgress func(sampleIndex int64, playAt time.Time)
	// OnSinkError 某一路写失败（不影响其他路）。
	OnSinkError func(playerID string, err error)
	// OnSinkDrop 某一路队列满丢帧。
	OnSinkDrop func(playerID string, frames int)
	// Clock 注入时钟（测试用）。
	Clock func() time.Time
	// Sleep 注入休眠（测试用，默认 time.Sleep）。
	Sleep func(time.Duration)

	mu     sync.Mutex
	active *fanout
}

// ErrNoSinks 没有可推送的 sink。
var ErrNoSinks = errors.New("distributor: no sinks")

// DefaultQueueFrames 每路队列深度（帧）。2s 缓冲 = 100 帧，留 20% 余量。
const DefaultQueueFrames = 120

// Run 消费 r 直到 EOF 或 ctx 取消。返回写出的帧数。
func (d *Distributor) Run(ctx context.Context, r io.Reader) (int, error) {
	if d.Targets.Len() == 0 {
		return 0, ErrNoSinks
	}
	rate := d.Rate
	if rate <= 0 {
		rate = 44100
	}
	frameSamples := d.FrameSamples
	if frameSamples <= 0 {
		frameSamples = DefaultFrameSamples
	}
	frameBytes := frameSamples * 2 * 2 // s16le 双声道
	depth := d.BufferDepth
	if depth <= 0 {
		depth = 2 * time.Second
	}
	now := d.Clock
	if now == nil {
		now = time.Now
	}
	sleep := d.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	timeline := d.Timeline
	if timeline == nil {
		timeline = player.NewTimeline(now(), rate, depth)
	}

	queue := int(depth / (time.Duration(frameSamples) * time.Second / time.Duration(rate)))
	if queue < 8 {
		queue = DefaultQueueFrames
	}
	queue += 8
	out := newFanout(queue, rate, frameSamples, d.OnSinkError, d.OnSinkDrop)
	d.mu.Lock()
	d.active = out
	d.mu.Unlock()
	defer func() {
		out.close()
		d.mu.Lock()
		if d.active == out {
			d.active = nil
		}
		d.mu.Unlock()
	}()

	buf := make([]byte, frameBytes)
	silence := make([]byte, frameBytes)
	index := d.StartSample
	frames := 0

	push := func(pcm []byte, idx int64, playAt time.Time) {
		targets, version := d.Targets.Snapshot()
		out.reconcile(targets, version)
		out.push(sink.Frame{SampleIndex: idx, PlayAt: playAt, Data: pcm})
	}

	for {
		if err := ctx.Err(); err != nil {
			return frames, err
		}
		n, readErr := io.ReadFull(r, buf)
		if readErr != nil && !errors.Is(readErr, io.ErrUnexpectedEOF) && !errors.Is(readErr, io.EOF) {
			return frames, readErr
		}
		last := errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF)
		if n > 0 && n < frameBytes {
			// 末尾半帧：补静音凑满，保证采样数永远是 882 的整数倍。
			copy(buf[n:], silence[:frameBytes-n])
		}

		// 欠载检测：这一帧本该已经开始播了，说明上游喂不上来。
		// 先把时间轴追平（插静音帧），再送这一帧，序号不倒退。
		if !last {
			for {
				playAt := timeline.At(uint64(index))
				if !playAt.Before(now()) {
					break
				}
				push(silence, index, playAt)
				index += int64(frameSamples)
				frames++
				if d.OnUnderrun != nil {
					d.OnUnderrun(1)
				}
			}
		}

		// 节奏控制：不允许领先播放时刻超过 BufferDepth。
		playAt := timeline.At(uint64(index))
		if ahead := playAt.Sub(now()) - depth; ahead > 0 {
			sleep(ahead)
		}
		if n > 0 {
			push(buf, index, playAt)
			frames++
			index += int64(frameSamples)
			if d.OnProgress != nil {
				d.OnProgress(index, playAt)
			}
		}
		if last {
			return frames, nil
		}
	}
}

// Skip 让某一路跳过后面的 d 时长（同步校正环用）。
//
// 只支持"追赶"：服务端无法让设备等待，只能丢帧。粒度为一整帧（20ms），
// 不足一帧按一帧算，与 ±20ms 的同步目标一致。
func (d *Distributor) Skip(playerID string, dur time.Duration) error {
	d.mu.Lock()
	out := d.active
	d.mu.Unlock()
	if out == nil {
		return errors.New("distributor: 未在播放")
	}
	return out.Skip(playerID, dur)
}

// Pad 让某一路插播 d 时长的静音（同步校正环用）。
//
// 与 Skip 互补：Skip 丢帧让落后的设备追上，Pad 插静音让超前的设备等一等。
// 实现方式是给该路累计一个采样偏移，后续帧的 SampleIndex 整体后移，
// 时间轴本身不动（其他成员不受影响）。
func (d *Distributor) Pad(playerID string, dur time.Duration) error {
	d.mu.Lock()
	out := d.active
	d.mu.Unlock()
	if out == nil {
		return errors.New("distributor: 未在播放")
	}
	return out.Pad(playerID, dur)
}

// Corrector 把自身暴露成同步校正执行者（player.CorrectionRunner）。
func (d *Distributor) Corrector() player.Corrector { return d }

// Stats 当前各路统计（监控与测试用）。
func (d *Distributor) Stats() []SinkStats {
	d.mu.Lock()
	out := d.active
	d.mu.Unlock()
	if out == nil {
		return nil
	}
	return out.Stats()
}

// fanout 每路一个写协程的分发器。
type fanout struct {
	mu      sync.Mutex
	writers map[string]*sinkWriter
	version uint64
	queue   int
	rate    int
	samples int
	onErr   func(string, error)
	onDrop  func(string, int)
	closed  bool
}

func newFanout(queue, rate, samples int, onErr func(string, error), onDrop func(string, int)) *fanout {
	return &fanout{
		writers: make(map[string]*sinkWriter),
		queue:   queue,
		rate:    rate,
		samples: samples,
		onErr:   onErr,
		onDrop:  onDrop,
	}
}

// reconcile 按目标集合的版本号对齐写协程。版本没变时是一次原子读，代价可忽略。
func (f *fanout) reconcile(targets []sink.Target, version uint64) {
	if version == f.version {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	want := make(map[string]sink.Target, len(targets))
	for _, t := range targets {
		want[t.PlayerID] = t
	}
	for id, w := range f.writers {
		if t, ok := want[id]; ok && t.Sink == w.sinkRef() {
			// 同一路只更新参数（EQ 调节立即生效），不重建协程。
			w.update(t)
			continue
		}
		close(w.ch)
		delete(f.writers, id)
	}
	for id, t := range want {
		if _, ok := f.writers[id]; ok {
			continue
		}
		w := newSinkWriter(t, f.queue, f.rate, f.samples, f.onErr, f.onDrop)
		f.writers[id] = w
		go w.run()
	}
	f.version = version
}

// push 把一帧投递给每一路。数据按路拷贝：读缓冲会被复用，且 EQ 就地处理。
func (f *fanout) push(frame sink.Frame) {
	// 持锁投递：reconcile 会关闭队列，出锁后并发写会打到已关闭的 channel。
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, w := range f.writers {
		data := make([]byte, len(frame.Data))
		copy(data, frame.Data)
		w.push(sink.Frame{SampleIndex: frame.SampleIndex, PlayAt: frame.PlayAt, Data: data})
	}
}

// Skip 给某一路记上待跳过的时长。
func (f *fanout) Skip(playerID string, dur time.Duration) error {
	if f == nil || dur <= 0 {
		return nil
	}
	f.mu.Lock()
	w := f.writers[playerID]
	f.mu.Unlock()
	if w == nil {
		return errors.New("distributor: 目标不存在")
	}
	w.skipFor(dur)
	return nil
}

// Pad 给某一路记上待插播的静音时长。
func (f *fanout) Pad(playerID string, dur time.Duration) error {
	if f == nil || dur <= 0 {
		return nil
	}
	f.mu.Lock()
	w := f.writers[playerID]
	f.mu.Unlock()
	if w == nil {
		return errors.New("distributor: 目标不存在")
	}
	w.padFor(dur)
	return nil
}

// close 关闭所有队列并等写协程退出。卡死的设备最多等 drainTimeout。
func (f *fanout) close() {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return
	}
	f.closed = true
	ws := make([]*sinkWriter, 0, len(f.writers))
	for id, w := range f.writers {
		close(w.ch)
		delete(f.writers, id)
		ws = append(ws, w)
	}
	f.mu.Unlock()
	deadline := time.After(drainTimeout)
	for _, w := range ws {
		select {
		case <-w.done:
		case <-deadline:
			return
		}
	}
}

// drainTimeout 收尾时最多等这么久让队列排空（设备彻底卡死时放弃）。
const drainTimeout = 3 * time.Second

// sinkWriter 单路写协程：独立队列 + 独立 WriteFrame 调用。
type sinkWriter struct {
	id      string
	ch      chan sink.Frame
	done    chan struct{}
	queue   int
	rate    int
	samples int
	onErr   func(string, error)
	onDrop  func(string, int)

	mu      sync.Mutex
	sink    sink.PushSink
	eq      sink.SampleProcessor
	delay   time.Duration
	skip    int64 // 待跳过的采样数
	pad     int64 // 待插播的静音采样数
	extra   int64 // 已插播的静音累计（后续帧 SampleIndex 整体后移）
	dropped int64
	skipped int64
	errs    int64
	lastErr error
}

func newSinkWriter(t sink.Target, queue, rate, samples int, onErr func(string, error), onDrop func(string, int)) *sinkWriter {
	return &sinkWriter{
		id:      t.PlayerID,
		ch:      make(chan sink.Frame, queue),
		done:    make(chan struct{}),
		queue:   queue,
		rate:    rate,
		samples: samples,
		onErr:   onErr,
		onDrop:  onDrop,
		sink:    t.Sink,
		eq:      t.EQ,
		delay:   t.Delay,
	}
}

func (w *sinkWriter) sinkRef() sink.PushSink {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sink
}

func (w *sinkWriter) update(t sink.Target) {
	w.mu.Lock()
	w.eq = t.EQ
	w.delay = t.Delay
	w.mu.Unlock()
}

// push 入队。队列满时丢最旧的帧：保住最新内容，别让整组跟着卡。
func (w *sinkWriter) push(f sink.Frame) {
	select {
	case w.ch <- f:
		return
	default:
	}
	select {
	case <-w.ch:
	default:
	}
	select {
	case w.ch <- f:
	default:
	}
	w.mu.Lock()
	w.dropped++
	w.mu.Unlock()
	if w.onDrop != nil {
		w.onDrop(w.id, 1)
	}
}

// skipFor 记上待跳过的时长（换算成采样，向上取整到整帧）。
func (w *sinkWriter) skipFor(d time.Duration) {
	n := int64(d.Seconds() * float64(w.rate))
	if n <= 0 {
		n = int64(w.samples)
	}
	w.mu.Lock()
	w.skip += n
	w.mu.Unlock()
}

// padFor 记上待插播的静音采样数。
func (w *sinkWriter) padFor(d time.Duration) {
	n := int64(d.Seconds() * float64(w.rate))
	if n <= 0 {
		n = int64(w.samples)
	}
	w.mu.Lock()
	w.pad += n
	w.mu.Unlock()
}

// takePad 取走待插播的静音采样数。
func (w *sinkWriter) takePad() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := w.pad
	w.pad = 0
	return n
}

// shouldDrop 本帧是否要丢（同步校正）。
func (w *sinkWriter) shouldDrop() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.skip <= 0 {
		return false
	}
	if w.skip >= int64(w.samples) {
		w.skip -= int64(w.samples)
	} else {
		// 不足一帧：整帧丢弃，避免半帧对齐把序号搞乱。
		w.skip = 0
	}
	w.skipped++
	return true
}

func (w *sinkWriter) run() {
	defer close(w.done)
	for f := range w.ch {
		if w.shouldDrop() {
			continue
		}
		if pad := w.takePad(); pad > 0 {
			w.writeSilence(pad, f.SampleIndex, f.PlayAt)
		}
		w.mu.Lock()
		eq := w.eq
		s := w.sink
		extra := w.extra
		w.mu.Unlock()
		if extra != 0 {
			f.SampleIndex += extra
		}
		if eq != nil {
			eq.ProcessInPlace(f.Data)
		}
		if err := s.WriteFrame(f); err != nil {
			w.mu.Lock()
			w.errs++
			w.lastErr = err
			w.mu.Unlock()
			if w.onErr != nil {
				w.onErr(w.id, err)
			}
		}
	}
}

// writeSilence 在 base 处插播 n 个采样帧的静音，按帧大小切块。
//
// 静音的 SampleIndex 接着当前播放头，后续帧整体后移 extra，时间轴本身不动。
func (w *sinkWriter) writeSilence(n int64, base int64, playAt time.Time) {
	w.mu.Lock()
	s := w.sink
	w.mu.Unlock()
	for n > 0 {
		chunk := n
		if chunk > int64(w.samples) {
			chunk = int64(w.samples)
		}
		w.mu.Lock()
		idx := base + w.extra
		w.extra += chunk
		w.mu.Unlock()
		data := make([]byte, chunk*4)
		if err := s.WriteFrame(sink.Frame{SampleIndex: idx, PlayAt: playAt, Data: data}); err != nil {
			w.mu.Lock()
			w.errs++
			w.lastErr = err
			w.mu.Unlock()
			if w.onErr != nil {
				w.onErr(w.id, err)
			}
			return
		}
		n -= chunk
	}
}

// Stats 某一路的统计（测试与监控用）。
type SinkStats struct {
	PlayerID string
	Dropped  int64
	Skipped  int64
	Errors   int64
	Queued   int
}

// Stats 返回所有路的统计。
func (f *fanout) Stats() []SinkStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SinkStats, 0, len(f.writers))
	for id, w := range f.writers {
		w.mu.Lock()
		out = append(out, SinkStats{PlayerID: id, Dropped: w.dropped, Skipped: w.skipped, Errors: w.errs, Queued: len(w.ch)})
		w.mu.Unlock()
	}
	return out
}
