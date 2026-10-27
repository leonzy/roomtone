package raat

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"songloft/internal/player/sink"
)

// screenState 设备屏显的当前状态（update_transport 单请求全量推送的
// 缓存字段，对齐 Roon：state+seek+now_playing 一并）。
type screenState struct {
	mu  sync.Mutex
	np  nowPlayingCache
	pos int // seek 秒
}

type nowPlayingCache struct {
	title, artist, album string
	length               int // 秒
}

// UpdateMetadata 实现 sink.MetadataSink：缓存 now_playing，随下一次
// update_transport 带出。
func (s *Sink) UpdateMetadata(m sink.NowPlaying) error {
	s.screen.mu.Lock()
	s.screen.np = nowPlayingCache{title: m.Title, artist: m.Artist, album: m.Album}
	s.screen.mu.Unlock()
	return s.pushUpdateTransport()
}

// UpdateProgress 实现 sink.ProgressSink：单请求全量推送
// （对齐 Roon：state+seek+now_playing 一并）。
func (s *Sink) UpdateProgress(pos, total time.Duration) error {
	s.screen.mu.Lock()
	s.screen.pos = int(pos.Seconds())
	s.screen.np.length = int(total.Seconds())
	s.screen.mu.Unlock()
	return s.pushUpdateTransport()
}

// UpdateArtwork 实现 sink.ArtworkSink：两步——先 type=3 裸二进制帧
// （图片字节，无 JSON），再 update_artwork 声明 mimetype。
// 空路径 → mimetype=null 清除（无裸帧）。
func (s *Sink) UpdateArtwork(path string) error {
	if path == "" {
		// 清除：mimetype=null，无裸帧
		_, err := s.ctrl.Request(context.Background(), "update_artwork", map[string]any{
			"mimetype": nil,
		}, nil)
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// 设备只认 JPEG；其它格式转码
	if ct := http.DetectContentType(raw); ct != "image/jpeg" {
		img, _, err := image.Decode(bytes.NewReader(raw))
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, nil); err != nil {
			return err
		}
		raw = buf.Bytes()
	}
	// ① type=3 裸二进制帧（body=图片字节，无 JSON）
	if err := s.ctrl.WriteRaw(3, raw); err != nil {
		return err
	}
	// ② update_artwork 声明 mimetype
	_, err = s.ctrl.Request(context.Background(), "update_artwork", map[string]any{
		"mimetype": "image/jpeg",
	}, nil)
	return err
}

// pushUpdateTransport 单请求全量推送（quiet:true + state + seek +
// now_playing + 权限位）。
func (s *Sink) pushUpdateTransport() error {
	s.screen.mu.Lock()
	np := s.screen.np
	seek := s.screen.pos
	s.screen.mu.Unlock()

	s.cbMu.Lock()
	state := "stopped"
	if s.started {
		state = "playing"
	}
	s.cbMu.Unlock()

	_, err := s.ctrl.Request(context.Background(), "update_transport", map[string]any{
		"quiet": true,
		"status": map[string]any{
			"loop":                "none",
			"shuffle":             false,
			"state":               state,
			"seek":                seek,
			"is_previous_allowed": true,
			"is_next_allowed":     true,
			"is_pause_allowed":    true,
			"is_play_allowed":     true,
			"is_seek_allowed":     true,
			"now_playing": map[string]any{
				"title":  np.title,
				"artist": np.artist,
				"album":  np.album,
				"length": np.length,
			},
		},
	}, nil)
	return err
}

// subscribeTransport 订阅设备遥控（transport 支持时；Open 末尾调用）。
// 通知 {"control":{"button":...}} → OnControl(button)；
// {"control":{"seek":{mode,seconds}}} → OnControl("seek", seconds)。
func (s *Sink) subscribeTransport() {
	if s.opts.OnControl == nil {
		return
	}
	s.ctrl.OnServerRequest = func(raw json.RawMessage) {
		var note struct {
			Control struct {
				Button string `json:"button"`
				Seek   *struct {
					Mode    string `json:"mode"`
					Seconds int    `json:"seconds"`
				} `json:"seek"`
			} `json:"control"`
		}
		if err := json.Unmarshal(raw, &note); err != nil {
			slog.Debug("raat: 服务器通知解析失败", "device", s.id, "err", err)
			return
		}
		if note.Control.Button != "" {
			s.recordControl(note.Control.Button, 0)
			if err := s.opts.OnControl(note.Control.Button, 0); err != nil {
				slog.Warn("raat: 设备反控失败", "device", s.id, "cmd", note.Control.Button, "err", err)
			}
			return
		}
		if note.Control.Seek != nil {
			seconds := float64(note.Control.Seek.Seconds)
			if note.Control.Seek.Mode == "relative" {
				// 相对 seek：以当前位置为基准（DeviceControl 的 seek 语义
				// 是绝对位置；相对量换算需要当前位置——经 OnControl 透传
				// 绝对语义，边界如实标注）
				slog.Debug("raat: relative seek 通知按绝对语义透传", "device", s.id, "seconds", seconds)
			}
			s.recordControl("seek", seconds)
			if err := s.opts.OnControl("seek", seconds); err != nil {
				slog.Warn("raat: 设备反控失败", "device", s.id, "cmd", "seek", "err", err)
			}
		}
	}
	// 订阅（响应忽略；后续通知走 OnServerRequest）
	if _, err := s.ctrl.Request(context.Background(), "subscribe_transport", nil, func(raw json.RawMessage, final bool) error {
		return nil // 订阅确认（{"status":"Success"} 等）忽略
	}); err != nil {
		slog.Debug("raat: subscribe_transport 失败", "device", s.id, "err", err)
	}
}

// recordControl 记录最近一次反控（测试断言用）。
func (s *Sink) recordControl(cmd string, val float64) {
	s.ctrlMu.Lock()
	s.lastControlCmd, s.lastControlVal = cmd, val
	s.ctrlMu.Unlock()
}
