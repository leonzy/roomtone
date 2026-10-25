package player

import (
	"context"
	"time"

	"songloft/internal/player/sink"
)

// GaplessPrefetchWindow 剩余时长小于它就预开下一曲解码器（B5-11）。
//
// 3s 是"够用且不浪费"的折中：ffmpeg 起一个解码器通常 20~80ms，给 3s
// 余量足够覆盖冷缓存与慢盘；窗口太大则会为"用户可能马上切歌"白占一个
// 解码进程（EQ 非 flat 时组还会因此多占一个预算槽位的理由）。
const GaplessPrefetchWindow = 3 * time.Second

// canGapless 组内所有 sink 都支持接续才能走 gapless。
//
// 混着走不行：一半接续一半重开，交界处两边的采样序号就对不上了，
// 听起来是"一半设备卡一下"，比整体降级更难排查。
func canGapless(targets []sink.Target) bool {
	if len(targets) == 0 {
		return false
	}
	for _, t := range targets {
		if _, ok := t.Sink.(sink.GaplessSink); !ok {
			return false
		}
	}
	return true
}

// openDecoder 取解码器：优先用预取好的那一份，否则现开。
//
// 预取命中时**必须**把 e.preload 清空：否则下一次 stopLocked 会去 Close
// 一个已经被播放协程接管的解码器，正在播的声音会被一起关掉。
func (e *Engine) openDecoder(ctx context.Context, track Track, position time.Duration, format Format) (Decoder, error) {
	e.mu.Lock()
	if e.preload != nil && e.preloadID == track.SongID && position == 0 {
		d := e.preload
		e.preload = nil
		e.preloadID = 0
		e.mu.Unlock()
		return d, nil
	}
	e.mu.Unlock()
	return e.deps.Decoder.Open(ctx, track, position, format)
}

// maybePrefetch 曲末预开下一曲。在每帧回调里调用，幂等。
func (e *Engine) maybePrefetch(sampleIndex int64, duration time.Duration, rate int) {
	if duration <= 0 || rate <= 0 {
		return
	}
	e.mu.Lock()
	plan, base := e.curPlan, e.planBase
	if e.preload != nil || !e.running {
		e.mu.Unlock()
		return
	}
	pos := plan.Position + time.Duration(float64(sampleIndex-base)/float64(rate)*float64(time.Second))
	remaining := duration - pos
	if remaining > GaplessPrefetchWindow {
		e.mu.Unlock()
		return
	}
	next, ok := e.queue.PeekNext()
	if !ok || next == plan.SongID {
		// 单曲循环/没有下一首：没必要预开，白占一个解码进程。
		e.mu.Unlock()
		return
	}
	format := e.format
	e.mu.Unlock()

	track, err := e.resolve(context.WithoutCancel(context.Background()), next)
	if err != nil {
		return
	}
	go func() {
		// 预热 ReplayGain 缓存：让读取标签的几十毫秒落在预取窗口里，
		// 而不是落在曲目交界上（gapless 的交界不允许额外停顿）。
		if e.deps.ReplayGain != nil {
			_, _ = e.deps.ReplayGain.GainDB(context.Background(), track.SongID)
		}
		d, err := e.deps.Decoder.Open(context.Background(), track, 0, format)
		if err != nil {
			// 预取失败不是错误：到点会走正常路径重新开一次。
			e.deps.Logger.Debug("prefetch failed", "song_id", next, "err", err)
			return
		}
		e.mu.Lock()
		// 期间用户可能已经切歌 / 停止：这时直接丢掉，别覆盖新的预取。
		if !e.running || e.preload != nil {
			e.mu.Unlock()
			_ = d.Close()
			return
		}
		e.preload = d
		e.preloadID = next
		e.mu.Unlock()
	}()
}
