// Package pipeline 是"单解码源 → 环形缓冲 → 多路分发"的实现。
//
// 一个组只有一个解码器（ffmpeg 一进程），PCM 以 20ms 为单位切帧后分发给所有
// PushSink；每帧携带统一时间轴上的绝对采样序号，sink 据此对齐。
//
// 欠载时补静音而不是等：等会让整组被拖偏，补静音只损失那一小段声音。
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"songloft/internal/player"
	"songloft/internal/player/sink"
)

// DefaultFrameSamples 20ms @ 44.1kHz = 882 采样。
const DefaultFrameSamples = 882

// FFmpegDecoderFactory 用 ffmpeg 解码。路径来自配置项 ffmpeg_path，
// 改了配置立即生效（每次 Open 读一次）。
//
// 解码器接口定义在 player 包（engine 只依赖接口，不依赖 ffmpeg），
// 这里只提供实现。
type FFmpegDecoderFactory struct {
	PathFn func() string
	Logger *slog.Logger
}

// NewFFmpegDecoderFactory 创建工厂。
func NewFFmpegDecoderFactory(pathFn func() string, logger *slog.Logger) *FFmpegDecoderFactory {
	if logger == nil {
		logger = slog.Default()
	}
	return &FFmpegDecoderFactory{PathFn: pathFn, Logger: logger}
}

// Open 启动 ffmpeg，输出 s16le。
func (f *FFmpegDecoderFactory) Open(ctx context.Context, track player.Track, startAt time.Duration, format sink.Format, eq player.EQProfile) (player.Decoder, error) {
	path := ""
	if f.PathFn != nil {
		path = strings.TrimSpace(f.PathFn())
	}
	if path == "" {
		return nil, errors.New("ffmpeg 未配置：请先在设置里指定 ffmpeg 路径")
	}
	if track.Path == "" && track.RemoteURL == "" {
		return nil, fmt.Errorf("song %d has no playable source", track.SongID)
	}

	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	if startAt > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", startAt.Seconds()))
	}
	if track.RemoteURL != "" {
		// 远程 URL：不落盘，直连。ffmpeg 只支持 HTTP/HTTPS 代理。
		args = append(args, "-reconnect", "1", "-reconnect_streamed", "1", "-i", track.RemoteURL)
	} else {
		args = append(args, "-i", track.Path)
	}
	args = append(args, "-vn")
	if filter := BuildFilterChain(eq); filter != "" {
		args = append(args, "-af", filter)
	}
	args = append(args,
		"-f", "s16le",
		"-acodec", "pcm_s16le",
		"-ar", fmt.Sprintf("%d", format.SampleRate),
		"-ac", fmt.Sprintf("%d", format.Channels),
		"pipe:1",
	)

	cmd := exec.CommandContext(ctx, path, args...)
	// 独立进程组：ctx 取消时 CommandContext 只 kill 父进程，
	// ffmpeg 自己 fork 的子进程会变孤儿继续吃 CPU。
	setProcessGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("ffmpeg stdout: %w", err)
	}
	stderr := &tailBuffer{limit: 4096}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}
	d := &ffmpegDecoder{cmd: cmd, stdout: stdout, stderr: stderr, logger: f.Logger}
	go func() {
		<-ctx.Done()
		d.kill()
	}()
	return d, nil
}

type ffmpegDecoder struct {
	cmd     *exec.Cmd
	stdout  io.ReadCloser
	stderr  *tailBuffer
	logger  *slog.Logger
	once    sync.Once
	waitErr error
}

func (d *ffmpegDecoder) Read(p []byte) (int, error) {
	n, err := d.stdout.Read(p)
	if errors.Is(err, io.EOF) {
		// 等 ffmpeg 真正退出，才能拿到退出码与 stderr 尾部。
		d.wait()
		if d.waitErr != nil {
			return n, d.waitErr
		}
	}
	return n, err
}

func (d *ffmpegDecoder) wait() {
	d.once.Do(func() {
		err := d.cmd.Wait()
		if err != nil {
			tail := strings.TrimSpace(d.stderr.String())
			d.waitErr = fmt.Errorf("ffmpeg exited: %w: %s", err, tail)
			if d.logger != nil {
				d.logger.Warn("ffmpeg decode failed", "err", err, "stderr", tail)
			}
		}
	})
}

func (d *ffmpegDecoder) kill() {
	killProcessGroup(d.cmd)
}

// Close 结束解码：先杀进程组，再回收。
func (d *ffmpegDecoder) Close() error {
	d.kill()
	d.wait()
	return nil
}

// Err 退出原因（EOF 后才有值）。
func (d *ffmpegDecoder) Err() error {
	d.wait()
	return d.waitErr
}

// tailBuffer 只保留最后 limit 字节的 stderr，防止长日志吃内存。
type tailBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		b.buf = b.buf[len(b.buf)-b.limit:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// fileExists 供测试与诊断用。
func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}
