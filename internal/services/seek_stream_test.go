package services

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"songloft/internal/testutil/executable"
)

// countingWriter 记录写入次数，用于断言「降级前绝不向响应写字节」这条不变量。
type countingWriter struct {
	writes int
	buf    bytes.Buffer
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	cw.writes++
	return cw.buf.Write(p)
}

// TestStreamSeekedMP3Unavailable 验证各种「无法开始」的情形都返回 ErrSeekStreamUnavailable
// 且一个字节都没写出——调用方正是靠这个契约无损降级为「从头完整提供文件」。
func TestStreamSeekedMP3Unavailable(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.mp3")
	if err := os.WriteFile(src, []byte("not really mp3"), 0644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	cases := []struct {
		name string
		cs   *CacheService
		opts SeekStreamOptions
	}{
		{
			name: "ffmpeg 未配置",
			cs:   &CacheService{},
			opts: SeekStreamOptions{SourcePath: src, StartSecond: 30},
		},
		{
			name: "ffmpeg 可执行文件不存在",
			cs:   newTestCacheService("", filepath.Join(dir, "nonexistent-ffmpeg"), ""),
			opts: SeekStreamOptions{SourcePath: src, StartSecond: 30},
		},
		{
			name: "ffmpeg 退出但零输出",
			cs:   newTestCacheService("", "/bin/true", ""),
			opts: SeekStreamOptions{SourcePath: src, StartSecond: 30},
		},
		{
			// StartSecond 0 只在 Normalize / Speed / ForceTranscode / Bitrate 至少有一个时合法，
			// 四者全无就是「没活干」，调用方应改走 http.ServeFile（支持 Range 且更快）。
			name: "起播秒数非正且无均衡/变速/转码理由",
			cs:   newTestCacheService("", executable.Echo(t), ""),
			opts: SeekStreamOptions{SourcePath: src, StartSecond: 0},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cw := &countingWriter{}
			err := tc.cs.StreamSeekedMP3(context.Background(), context.Background(), cw, tc.opts)
			if !errors.Is(err, ErrSeekStreamUnavailable) {
				t.Fatalf("err = %v, want ErrSeekStreamUnavailable", err)
			}
			if cw.writes != 0 {
				t.Errorf("写入次数 = %d，期望 0（降级前不得写响应）", cw.writes)
			}
		})
	}
}

// TestStreamSeekedMP3FFmpegArgs 用 /bin/echo 冒充 ffmpeg，把参数契约钉住：
// 缺 -map 0:a:0 会让双音轨源（.mka，songloft-org/songloft#298）直接失败，
// 缺 -write_xing 0 会让音箱读到错误时长。两者都只会表现为静默降级，极难归因。
func TestStreamSeekedMP3FFmpegArgs(t *testing.T) {
	dir := t.TempDir()

	run := func(t *testing.T, name string) string {
		t.Helper()
		src := filepath.Join(dir, name)
		if err := os.WriteFile(src, []byte("x"), 0644); err != nil {
			t.Fatalf("write src: %v", err)
		}
		cs := newTestCacheService("", executable.Echo(t), "")
		var buf bytes.Buffer
		if err := cs.StreamSeekedMP3(context.Background(), context.Background(), &buf, SeekStreamOptions{
			SourcePath: src, StartSecond: 61.5, RemainingSecond: 100,
		}); err != nil {
			t.Fatalf("StreamSeekedMP3: %v", err)
		}
		return buf.String()
	}

	t.Run("mp3 源走无损 copy", func(t *testing.T) {
		out := run(t, "a.mp3")
		for _, want := range []string{"-ss 61.500", "-map 0:a:0", "-vn", "-codec:a copy", "-write_xing 0", "-f mp3", "pipe:1"} {
			if !bytes.Contains([]byte(out), []byte(want)) {
				t.Errorf("ffmpeg 参数缺 %q，实际: %s", want, out)
			}
		}
		// input seek：-ss 必须在 -i 之前，否则退化为解码丢弃前 N 秒
		if idxSS, idxI := bytes.Index([]byte(out), []byte("-ss")), bytes.Index([]byte(out), []byte("-i")); idxSS > idxI {
			t.Errorf("-ss 出现在 -i 之后（非 input seek）: %s", out)
		}
	})

	t.Run("非 mp3 源按 CBR 重编码", func(t *testing.T) {
		out := run(t, "a.flac")
		if !bytes.Contains([]byte(out), []byte("-codec:a libmp3lame")) {
			t.Errorf("期望 libmp3lame 重编码，实际: %s", out)
		}
		// 必须是 CBR：无 Xing 头的流只能按码率估时长，VBR 会让音箱把时长估短约 7%
		if !bytes.Contains([]byte(out), []byte("-b:a 320k")) {
			t.Errorf("期望 CBR -b:a 320k，实际: %s", out)
		}
	})
}

// TestStreamSeekedMP3NormalizeArgs 钉住「边转边发音量均衡」的参数契约
// （songloft-org/songloft-plugin-miot#61）：
//   - mp3 源也必须重编码，走 copy 的话 loudnorm 滤镜静默失效、听感与落盘产物不一致；
//   - StartSecond 为 0 时不能出现 -ss，纯均衡流应从头开始；
//   - StartSecond > 0 时 seek 与均衡由同一条 ffmpeg 一起做。
func TestStreamSeekedMP3NormalizeArgs(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.mp3")
	if err := os.WriteFile(src, []byte("x"), 0644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	run := func(t *testing.T, startSecond float64) string {
		t.Helper()
		cs := newTestCacheService("", executable.Echo(t), "")
		var buf bytes.Buffer
		if err := cs.StreamSeekedMP3(context.Background(), context.Background(), &buf, SeekStreamOptions{
			SourcePath: src, StartSecond: startSecond, RemainingSecond: 100, Normalize: true,
		}); err != nil {
			t.Fatalf("StreamSeekedMP3: %v", err)
		}
		return buf.String()
	}

	t.Run("纯均衡不带 seek", func(t *testing.T) {
		out := run(t, 0)
		for _, want := range []string{
			"-af " + loudnormFilter, // 与落盘转码共用同一滤镜串，否则两条路径响度不同
			"-codec:a libmp3lame",   // mp3 源也不能 copy
			"-b:a 320k",             // pipe 无法回写 Xing，只有 CBR 能让音箱估准时长
			"-map 0:a:0", "-vn", "-write_xing 0", "-f mp3", "pipe:1",
		} {
			if !bytes.Contains([]byte(out), []byte(want)) {
				t.Errorf("ffmpeg 参数缺 %q，实际: %s", want, out)
			}
		}
		if bytes.Contains([]byte(out), []byte("-ss")) {
			t.Errorf("StartSecond=0 不该出现 -ss，实际: %s", out)
		}
	})

	t.Run("均衡叠加 seek", func(t *testing.T) {
		out := run(t, 42)
		for _, want := range []string{"-ss 42.000", "-af " + loudnormFilter, "-codec:a libmp3lame"} {
			if !bytes.Contains([]byte(out), []byte(want)) {
				t.Errorf("ffmpeg 参数缺 %q，实际: %s", want, out)
			}
		}
		if idxSS, idxI := bytes.Index([]byte(out), []byte("-ss")), bytes.Index([]byte(out), []byte("-i")); idxSS > idxI {
			t.Errorf("-ss 出现在 -i 之后（非 input seek）: %s", out)
		}
	})
}

// TestStreamSeekedMP3SpeedArgs 钉住「变速播放（atempo 滤镜）」的参数契约：
//   - Speed!=1.0 必须重编码（atempo 是时域滤镜，copy 下不生效），mp3 源也不能走 copy 快路径；
//   - atempo 滤镜串以 atempo=N.NNN 形式出现在 -af 上，与 loudnorm 同开时用逗号拼接，atempo 在前；
//   - StartSecond=0 + Speed!=1.0 是合法组合（纯变速流，从头起播但变速），不该被拒绝。
func TestStreamSeekedMP3SpeedArgs(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.mp3")
	if err := os.WriteFile(src, []byte("x"), 0644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	t.Run("纯变速从头起播", func(t *testing.T) {
		cs := newTestCacheService("", executable.Echo(t), "")
		var buf bytes.Buffer
		if err := cs.StreamSeekedMP3(context.Background(), context.Background(), &buf, SeekStreamOptions{
			SourcePath: src, StartSecond: 0, RemainingSecond: 100, Speed: 1.5,
		}); err != nil {
			t.Fatalf("StreamSeekedMP3: %v", err)
		}
		out := buf.String()
		for _, want := range []string{"-af atempo=1.500", "-codec:a libmp3lame", "-b:a 320k", "-f mp3", "pipe:1"} {
			if !bytes.Contains([]byte(out), []byte(want)) {
				t.Errorf("ffmpeg 参数缺 %q，实际: %s", want, out)
			}
		}
		if bytes.Contains([]byte(out), []byte("-codec:a copy")) {
			t.Errorf("atempo 必须重编码，却走了 copy: %s", out)
		}
		if bytes.Contains([]byte(out), []byte("-ss")) {
			t.Errorf("StartSecond=0 不该出现 -ss，实际: %s", out)
		}
	})

	t.Run("变速叠加 seek", func(t *testing.T) {
		cs := newTestCacheService("", executable.Echo(t), "")
		var buf bytes.Buffer
		if err := cs.StreamSeekedMP3(context.Background(), context.Background(), &buf, SeekStreamOptions{
			SourcePath: src, StartSecond: 42, RemainingSecond: 100, Speed: 2.0,
		}); err != nil {
			t.Fatalf("StreamSeekedMP3: %v", err)
		}
		out := buf.String()
		for _, want := range []string{"-ss 42.000", "-af atempo=2.000", "-codec:a libmp3lame"} {
			if !bytes.Contains([]byte(out), []byte(want)) {
				t.Errorf("ffmpeg 参数缺 %q，实际: %s", want, out)
			}
		}
	})

	t.Run("变速叠加均衡滤镜顺序", func(t *testing.T) {
		cs := newTestCacheService("", executable.Echo(t), "")
		var buf bytes.Buffer
		if err := cs.StreamSeekedMP3(context.Background(), context.Background(), &buf, SeekStreamOptions{
			SourcePath: src, StartSecond: 0, RemainingSecond: 100, Normalize: true, Speed: 1.25,
		}); err != nil {
			t.Fatalf("StreamSeekedMP3: %v", err)
		}
		out := buf.String()
		// atempo 必须在 loudnorm 之前：先变速再均衡，响度分析基于变速后的信号
		idxAtempo := bytes.Index([]byte(out), []byte("atempo=1.250"))
		idxLoudnorm := bytes.Index([]byte(out), []byte("loudnorm="))
		if idxAtempo < 0 || idxLoudnorm < 0 {
			t.Fatalf("缺 atempo 或 loudnorm 滤镜，实际: %s", out)
		}
		if idxAtempo > idxLoudnorm {
			t.Errorf("atempo 应在 loudnorm 之前，实际: %s", out)
		}
		// 两者必须由逗号拼接进同一条 -af，而非两条 -af
		if n := bytes.Count([]byte(out), []byte("-af ")); n != 1 {
			t.Errorf("-af 出现 %d 次，期望 1（变速与均衡应由同一条 -af 拼接）: %s", n, out)
		}
	})
}

// TestStreamSeekedMP3TranscodeArgs 钉住「普通转码播放也边转边发」的参数契约
// （songloft-org/songloft#442）：
//   - ForceTranscode 让 StartSecond=0、不均衡、不变速、未指定码率的纯转码流也能开始，
//     否则守卫会判它「没活干」，那条路就只剩阻塞整首转码（首字节 = 整首转码墙钟时间）；
//   - ForceTranscode 必须同时禁掉 copy 快路径：伪 mp3（扩展名 .mp3 内容是 WebM/Opus，
//     songloft-org/songloft#300）走 copy 会被 mp3 muxer 拒收 → 零输出 → 降级，白起一个 ffmpeg；
//   - Bitrate 按请求出 CBR（?quality=128/192/320），且 mp3 源也不能 copy——copy 保留的是
//     源码率，与请求的 quality 不符。
func TestStreamSeekedMP3TranscodeArgs(t *testing.T) {
	dir := t.TempDir()

	run := func(t *testing.T, name string, opts SeekStreamOptions) string {
		t.Helper()
		src := filepath.Join(dir, name)
		if err := os.WriteFile(src, []byte("x"), 0644); err != nil {
			t.Fatalf("write src: %v", err)
		}
		opts.SourcePath = src
		cs := newTestCacheService("", executable.Echo(t), "")
		var buf bytes.Buffer
		if err := cs.StreamSeekedMP3(context.Background(), context.Background(), &buf, opts); err != nil {
			t.Fatalf("StreamSeekedMP3: %v", err)
		}
		return buf.String()
	}

	t.Run("纯转码从头起播", func(t *testing.T) {
		out := run(t, "lossless.flac", SeekStreamOptions{RemainingSecond: 240, ForceTranscode: true})
		for _, want := range []string{"-codec:a libmp3lame", "-b:a 320k", "-map 0:a:0", "-vn", "-write_xing 0", "-f mp3", "pipe:1"} {
			if !bytes.Contains([]byte(out), []byte(want)) {
				t.Errorf("ffmpeg 参数缺 %q，实际: %s", want, out)
			}
		}
		if bytes.Contains([]byte(out), []byte("-ss")) {
			t.Errorf("StartSecond=0 不该出现 -ss，实际: %s", out)
		}
		if bytes.Contains([]byte(out), []byte("-af")) {
			t.Errorf("未请求均衡/变速却出现滤镜: %s", out)
		}
	})

	// 伪 mp3：扩展名骗过了 NormalizeFormat，只有 ForceTranscode 能拦住 copy 快路径。
	t.Run("mp3 扩展名叠加 ForceTranscode 仍重编码", func(t *testing.T) {
		out := run(t, "fake.mp3", SeekStreamOptions{RemainingSecond: 240, ForceTranscode: true})
		if !bytes.Contains([]byte(out), []byte("-codec:a libmp3lame")) {
			t.Errorf("期望重编码，实际: %s", out)
		}
		if bytes.Contains([]byte(out), []byte("-codec:a copy")) {
			t.Errorf("ForceTranscode 下仍走了 copy（伪 mp3 会被 mp3 muxer 拒收）: %s", out)
		}
	})

	// ?quality= 的各档码率都要如实出现在 -b:a 上，否则客户端拿到的码率是假的。
	for _, bitrate := range []int{128, 192, 320} {
		t.Run(fmt.Sprintf("quality %dk 按请求出 CBR", bitrate), func(t *testing.T) {
			out := run(t, fmt.Sprintf("q%d.flac", bitrate), SeekStreamOptions{
				RemainingSecond: 240, ForceTranscode: true, Bitrate: bitrate,
			})
			want := fmt.Sprintf("-b:a %dk", bitrate)
			if !bytes.Contains([]byte(out), []byte(want)) {
				t.Errorf("ffmpeg 参数缺 %q，实际: %s", want, out)
			}
		})
	}

	// Bitrate 单独就是一个充分理由：调用方只想改码率、忘了 ForceTranscode 也不该被守卫误拒。
	t.Run("仅 Bitrate 也能开始且 mp3 源不 copy", func(t *testing.T) {
		out := run(t, "src.mp3", SeekStreamOptions{RemainingSecond: 240, Bitrate: 128})
		if !bytes.Contains([]byte(out), []byte("-b:a 128k")) {
			t.Errorf("期望 -b:a 128k，实际: %s", out)
		}
		if bytes.Contains([]byte(out), []byte("-codec:a copy")) {
			t.Errorf("指定码率时 mp3 源仍走 copy（码率会是源码率而非请求值）: %s", out)
		}
	})

	// 回归保护：不带转码理由的纯 seek 必须保留 mp3 源的 copy 快路径（实测整首 0.14s、近零 CPU）。
	t.Run("纯 seek 的 mp3 源仍走 copy", func(t *testing.T) {
		out := run(t, "seekonly.mp3", SeekStreamOptions{StartSecond: 30, RemainingSecond: 240})
		if !bytes.Contains([]byte(out), []byte("-codec:a copy")) {
			t.Errorf("纯 seek 的 mp3 源应走 copy 快路径，实际: %s", out)
		}
	})

	t.Run("转码叠加 seek 与变速由同一条 ffmpeg 完成", func(t *testing.T) {
		out := run(t, "combo.flac", SeekStreamOptions{
			StartSecond: 42, RemainingSecond: 240, ForceTranscode: true, Bitrate: 192, Speed: 1.5,
		})
		for _, want := range []string{"-ss 42.000", "-af atempo=1.500", "-codec:a libmp3lame", "-b:a 192k"} {
			if !bytes.Contains([]byte(out), []byte(want)) {
				t.Errorf("ffmpeg 参数缺 %q，实际: %s", want, out)
			}
		}
		if n := bytes.Count([]byte(out), []byte("pipe:1")); n != 1 {
			t.Errorf("pipe:1 出现 %d 次，期望 1（转码/seek/变速应由同一条 ffmpeg 完成）: %s", n, out)
		}
	})
}

// TestSeekStreamTimeout 钉住倍速下硬超时的换算：
// 2x 下 ffmpeg 实际只需一半墙钟时间就能转完写完，超时应显著短于 1x；0.5x 反而要翻倍。
// 漏掉这一步会让 0.5x 播放在音频还没转完时被硬超时掐断，表现为「变速播放到一半突然断掉」。
func TestSeekStreamTimeout(t *testing.T) {
	const remaining = 200.0
	t1 := seekStreamTimeout(remaining, 1.0)
	t2 := seekStreamTimeout(remaining, 2.0)
	tHalf := seekStreamTimeout(remaining, 0.5)

	// 2x 超时应明显短于 1x（约一半，加上固定 grace）
	if t2 >= t1 {
		t.Errorf("2x 超时 %v 不小于 1x %v，倍速换算未生效", t2, t1)
	}
	if t2 >= t1-time.Minute {
		t.Errorf("2x 超时 %v 与 1x %v 差距太小，期望约减半", t2, t1)
	}
	// 0.5x 超时应明显长于 1x（约翻倍）
	if tHalf <= t1 {
		t.Errorf("0.5x 超时 %v 不大于 1x %v，倍速换算未生效", tHalf, t1)
	}
	// 剩余时长未知时退化成固定兜底，不受倍速影响
	if got := seekStreamTimeout(0, 2.0); got != seekStreamUnknownDurationTimeout {
		t.Errorf("未知时长下超时应为 %v，实际 %v", seekStreamUnknownDurationTimeout, got)
	}
	// speed=1（不变速）与 speed=0（未指定）行为一致
	if seekStreamTimeout(remaining, 0) != t1 {
		t.Errorf("speed=0 应与 speed=1.0 超时一致")
	}
}

// ctx 被 playActivity.Activate 提前取消（用户切歌，让 ffmpeg 让位），但 connCtx（真实连接）
// 仍存活时，必须返回 ErrSeekStreamAborted，而不是把这次截断当作"正常结束"。
//
// 修复前的行为是 `if ctx.Err() != nil { return nil }`——不区分两种取消来源，会让客户端把
// 截断的音频误判为下载完整并永久缓存，此后再也不会重新请求转码。
func TestStreamSeekedMP3AbortedByActivity(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "tone.mp3")
	gen := exec.Command(ffmpegPath, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=30", "-codec:a", "libmp3lame", "-y", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("generate sample mp3 failed: %v (%s)", err, out)
	}

	cs := newTestCacheService("", ffmpegPath, "")
	ctx, cancelCtx := context.WithCancel(context.Background())
	connCtx := context.Background() // 模拟底层 TCP 连接仍存活，客户端仍在读

	// 收到第一个字节（即 io.Copy 真正开始）之后才 cancel，避免与 ffmpeg 启动耗时竞争——
	// 否则 cancel 可能在 Peek(1) 拿到首字节之前就杀掉进程，落到 ErrSeekStreamUnavailable
	// 分支而不是本测试要验证的中途取消分支。之后的固定小睡把 io.Copy 拉长到能让
	// cancel 生效于「已写出部分字节、尚未整体结束」的中间状态。
	w := newStallingWriter(10 * time.Millisecond)
	go func() {
		<-w.firstWrite
		cancelCtx() // 模拟 playActivity.Activate：只取消 ctx，不动 connCtx
	}()

	err = cs.StreamSeekedMP3(ctx, connCtx, w, SeekStreamOptions{
		SourcePath: src, StartSecond: 0, RemainingSecond: 30, Normalize: true,
	})
	if !errors.Is(err, ErrSeekStreamAborted) {
		t.Fatalf("err = %v, want ErrSeekStreamAborted", err)
	}
	if w.writes == 0 {
		t.Error("writes = 0，期望已经写出部分字节（否则应该走 ErrSeekStreamUnavailable 降级路径）")
	}
}

// stallingWriter 每次 Write 都小睡固定时长，用于在测试里把 io.Copy 循环拉长到
// 能被外部 goroutine 中途 cancel 的量级；firstWrite 在第一次 Write 时关闭，
// 供调用方等待"复制已经真正开始"再触发 cancel。
type stallingWriter struct {
	delay      time.Duration
	writes     int
	firstOnce  sync.Once
	firstWrite chan struct{}
}

func newStallingWriter(delay time.Duration) *stallingWriter {
	return &stallingWriter{delay: delay, firstWrite: make(chan struct{})}
}

func (w *stallingWriter) Write(p []byte) (int, error) {
	w.writes++
	w.firstOnce.Do(func() { close(w.firstWrite) })
	time.Sleep(w.delay)
	return len(p), nil
}

// TestSeekStreamUnknownDurationTimeout 钉住「剩余时长未知时的硬超时不能退化成 5 分钟」。
//
// 5 分钟对整首流不是宽限而是硬上限：客户端边播边读时恰好只送出 5 分钟音频就被 CommandContext
// 杀掉，而响应正常闭合、客户端以为播完了。songs.duration == 0 是常态（远程歌曲元数据未刷新、
// 本地文件 tag 与 ffprobe 都拿不到时长），叠上「均衡流每次播放都走这条路」就会真踩到。
func TestSeekStreamUnknownDurationTimeout(t *testing.T) {
	if seekStreamUnknownDurationTimeout <= seekStreamGrace {
		t.Fatalf("未知时长的硬超时 %v 不得小于等于 seekStreamGrace %v——那样长音频会被静默截断",
			seekStreamUnknownDurationTimeout, seekStreamGrace)
	}
	// 至少要覆盖有声书量级；这里取 1 小时作为下界，避免日后有人调小到分钟级
	if seekStreamUnknownDurationTimeout < time.Hour {
		t.Errorf("未知时长的硬超时 %v 太短，覆盖不了长音频", seekStreamUnknownDurationTimeout)
	}
}

// TestStreamSeekedMP3NormalizeRealFFmpeg 用真 ffmpeg 端到端验证纯均衡流：
// 输出是可解的 MP3，且时长与源一致（没被 -ss 或滤镜吃掉开头）。
func TestStreamSeekedMP3NormalizeRealFFmpeg(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "tone.mp3")
	gen := exec.Command(ffmpegPath, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=6", "-codec:a", "libmp3lame", "-y", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("generate sample mp3 failed: %v (%s)", err, out)
	}

	cs := newTestCacheService("", ffmpegPath, "")
	var buf bytes.Buffer
	if err := cs.StreamSeekedMP3(context.Background(), context.Background(), &buf, SeekStreamOptions{
		SourcePath: src, StartSecond: 0, RemainingSecond: 6, Normalize: true,
	}); err != nil {
		t.Fatalf("StreamSeekedMP3: %v", err)
	}

	out := buf.Bytes()
	if len(out) == 0 {
		t.Fatal("输出为空")
	}
	isID3 := bytes.HasPrefix(out, []byte("ID3"))
	isFrameSync := len(out) >= 2 && out[0] == 0xFF && out[1]&0xE0 == 0xE0
	if !isID3 && !isFrameSync {
		t.Errorf("输出不是 MP3 流: % x", out[:min(4, len(out))])
	}
	// 320k CBR 的 6 秒约 240 KB；只要明显大于「被截掉开头」的量级即可，不做精确断言。
	if len(out) < 100*1024 {
		t.Errorf("输出仅 %d 字节，纯均衡流不该丢开头", len(out))
	}
}

// TestStreamSeekedMP3RealFFmpeg 用真 ffmpeg 端到端验证输出确实是从 StartSecond 起的 MP3。
func TestStreamSeekedMP3RealFFmpeg(t *testing.T) {
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not available")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "tone.mp3")
	gen := exec.Command(ffmpegPath, "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=6", "-codec:a", "libmp3lame", "-y", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("generate sample mp3 failed: %v (%s)", err, out)
	}

	cs := newTestCacheService("", ffmpegPath, "")
	var buf bytes.Buffer
	if err := cs.StreamSeekedMP3(context.Background(), context.Background(), &buf, SeekStreamOptions{
		SourcePath: src, StartSecond: 4, RemainingSecond: 2,
	}); err != nil {
		t.Fatalf("StreamSeekedMP3: %v", err)
	}

	out := buf.Bytes()
	if len(out) == 0 {
		t.Fatal("输出为空")
	}
	// 合法 MP3 流开头：ID3v2 头（ffmpeg mp3 muxer 默认写元数据）或直接是帧同步头
	// （0xFF 后接 0xE0 的高 3 位，即 11 个 1 bit）。
	isID3 := bytes.HasPrefix(out, []byte("ID3"))
	isFrameSync := len(out) >= 2 && out[0] == 0xFF && out[1]&0xE0 == 0xE0
	if !isID3 && !isFrameSync {
		t.Errorf("输出不是 MP3 流: % x", out[:min(4, len(out))])
	}

	full, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read src: %v", err)
	}
	// 6 秒里跳掉 4 秒，剩余字节应显著少于整首（同码率下约 1/3）
	if len(out) >= len(full) {
		t.Errorf("seek 后字节数 %d 不小于整首 %d，input seek 可能未生效", len(out), len(full))
	}
}
