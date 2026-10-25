package pipeline

import (
	"context"
	"errors"
	"io"
	"time"

	"songloft/internal/player"
	"songloft/internal/player/sink"
)

// Distributor 把一条 PCM 流切成 20ms 帧分发给所有 PushSink。
//
// 关键性质（都有测试守着）：
//   - 每帧固定 FrameSamples 个采样，字节数 = FrameSamples × 4（16bit 双声道）；
//   - SampleIndex 连续单调，跨曲目也不归零（无缝续播的地基）；
//   - 欠载补静音：落后于时间轴时插入静音帧把序号追平，**不推迟后续帧**；
//   - 发送节奏按"领先播放时刻不超过 BufferDepth"，不会一口气灌满设备缓冲。
type Distributor struct {
	Rate         int
	FrameSamples int
	Sinks        []sink.PushSink
	Timeline     *player.Timeline
	// BufferDepth 允许领先播放时刻多少（默认 2s，与 RAOP latency 对齐）。
	BufferDepth time.Duration
	// StartSample 起始采样序号（seek 后接着原时间轴）。
	StartSample int64
	// OnUnderrun 欠载回调，参数是本次补的帧数。
	OnUnderrun func(frames int)
	// OnProgress 每帧回调（用于上报水位 / 位置）。
	OnProgress func(sampleIndex int64, playAt time.Time)
	// Clock 注入时钟（测试用）。
	Clock func() time.Time
	// Sleep 注入休眠（测试用，默认 time.Sleep）。
	Sleep func(time.Duration)
}

// ErrNoSinks 没有可推送的 sink。
var ErrNoSinks = errors.New("distributor: no sinks")

// Run 消费 r 直到 EOF 或 ctx 取消。返回写出的帧数。
func (d *Distributor) Run(ctx context.Context, r io.Reader) (int, error) {
	if len(d.Sinks) == 0 {
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

	buf := make([]byte, frameBytes)
	silence := make([]byte, frameBytes)
	index := d.StartSample
	frames := 0

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
				if err := d.write(ctx, silence, index, playAt); err != nil {
					return frames, err
				}
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
			if err := d.write(ctx, buf, index, playAt); err != nil {
				return frames, err
			}
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

func (d *Distributor) write(ctx context.Context, pcm []byte, index int64, playAt time.Time) error {
	frame := sink.Frame{SampleIndex: index, PlayAt: playAt, Data: pcm}
	var firstErr error
	for _, s := range d.Sinks {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.WriteFrame(frame); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
