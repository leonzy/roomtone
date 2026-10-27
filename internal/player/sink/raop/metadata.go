package raop

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"net/http"
	"os"
	"time"

	"songloft/internal/player/sink"
)

// 设备屏显三推送（元数据 / 进度 / 封面），逐字对齐 Roon AirPlayClientV1.cs
// 的 UpdateMetadata / UpdatePosition / UpdateImage（2092-2167）：
//
//   - 全部走 RTSP SET_PARAMETER rtsp://<host>/<session>；
//   - 头 RTP-Info: rtptime=<当前可闻采样的 RTP 时间戳>；
//   - 元数据：application/x-dmap-tagged，mlit{mikd=2,asal,asar,asaa,minm}；
//   - 进度：text/parameters，progress: start/cur/end\r\n（RTP 时间戳）；
//   - 封面：image/jpeg 原始字节（非 JPEG 输入转码），空路径 image/none 清屏。
//
// "当前可闻采样" = anchor + (now - startAt) × rate（播放中）。Roon 侧的
// 对应量是 _rtp_time - 88200（88200 = 2s × 44100，即设备缓冲延迟）。

// audibleSampleLocked 当前正在发声的采样（播放中）。
func (s *Sink) audibleSampleLocked() int64 {
	if s.startAt.IsZero() {
		return s.anchor
	}
	return s.anchor + int64(time.Since(s.startAt).Seconds()*float64(s.opts.SampleRate))
}

// UpdateMetadata 实现 sink.MetadataSink。
func (s *Sink) UpdateMetadata(m sink.NowPlaying) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rtsp == nil {
		return nil
	}
	var e dmapEncoder
	e.Begin("mlit")
	e.WriteU8("mikd", 2)
	e.WriteString("asal", m.Album)
	e.WriteString("asar", m.Artist)
	e.WriteString("asaa", m.Artist)
	e.WriteString("minm", m.Title)
	e.End("mlit")
	body, err := e.Bytes()
	if err != nil {
		return err
	}
	return s.setParameterDMAPLocked(context.Background(), body)
}

// UpdateProgress 实现 sink.ProgressSink。
func (s *Sink) UpdateProgress(pos, total time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rtsp == nil {
		return nil
	}
	cur := s.tsLocked(s.audibleSampleLocked())
	rate := int64(s.opts.SampleRate)
	posSamples := int64(pos.Seconds() * float64(rate))
	totalSamples := int64(total.Seconds() * float64(rate))
	start := cur - uint32(posSamples) // uint32 环回，与 Roon 的 uint 运算一致
	end := start + uint32(totalSamples)
	body := []byte("progress: " + uitoa(uint64(start)) + "/" + uitoa(uint64(cur)) + "/" + uitoa(uint64(end)) + "\r\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.rtsp.Do(ctx, "SET_PARAMETER", s.uri(), map[string]string{
		"Content-Type": "text/parameters",
		"RTP-Info":     "rtptime=" + uitoa(uint64(cur)),
	}, body)
	return err
}

// UpdateArtwork 实现 sink.ArtworkSink。path 为空时发 image/none 清屏。
func (s *Sink) UpdateArtwork(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rtsp == nil {
		return nil
	}
	if path == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := s.rtsp.Do(ctx, "SET_PARAMETER", s.uri(), map[string]string{
			"Content-Type": "image/none",
			"RTP-Info":     "rtptime=" + uitoa(uint64(s.tsLocked(s.audibleSampleLocked()))),
		}, nil)
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	// 设备只认 JPEG；其它格式转码（对齐 Roon 发送端总是准备好的 JPEG）。
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = s.rtsp.Do(ctx, "SET_PARAMETER", s.uri(), map[string]string{
		"Content-Type": "image/jpeg",
		"RTP-Info":     "rtptime=" + uitoa(uint64(s.tsLocked(s.audibleSampleLocked()))),
	}, raw)
	return err
}

func (s *Sink) setParameterDMAPLocked(ctx context.Context, body []byte) error {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := s.rtsp.Do(cctx, "SET_PARAMETER", s.uri(), map[string]string{
		"Content-Type": "application/x-dmap-tagged",
		"RTP-Info":     "rtptime=" + uitoa(uint64(s.tsLocked(s.audibleSampleLocked()))),
	}, body)
	return err
}

// uitoa 避免引入 strconv 到热路径的小工具（strconv.Itoa 亦可）。
func uitoa(v uint64) string {
	// 最长 20 位 + 符号，直接用最简实现。
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
