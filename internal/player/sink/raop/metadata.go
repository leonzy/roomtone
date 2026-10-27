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
	return s.setParameterDMAP(context.Background(), body)
}

// UpdateProgress 实现 sink.ProgressSink。
func (s *Sink) UpdateProgress(pos, total time.Duration) error {
	cl, uri, cur, rate := s.metadataTarget()
	if cl == nil {
		return nil
	}
	start := cur - uint32(pos.Seconds()*float64(rate))
	end := start + uint32(total.Seconds()*float64(rate))
	body := []byte("progress: " + uitoa(uint64(start)) + "/" + uitoa(uint64(cur)) + "/" + uitoa(uint64(end)) + "\r\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cl.Do(ctx, "SET_PARAMETER", uri, map[string]string{"Content-Type": "text/parameters", "RTP-Info": "rtptime=" + uitoa(uint64(cur))}, body)
	return err
}

// UpdateArtwork 实现 sink.ArtworkSink。path 为空时发 image/none 清屏。
func (s *Sink) UpdateArtwork(path string) error {
	if path == "" {
		return s.setDisplayParameter(context.Background(), "image/none", nil)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
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
	return s.setDisplayParameter(context.Background(), "image/jpeg", raw)
}

func (s *Sink) setParameterDMAP(ctx context.Context, body []byte) error {
	return s.setDisplayParameter(ctx, "application/x-dmap-tagged", body)
}

// Snapshot only; a slow screen response must not hold the PCM writer mutex.
func (s *Sink) metadataTarget() (*Client, string, uint32, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rtsp == nil {
		return nil, "", 0, 0
	}
	return s.rtsp, s.uri(), s.tsLocked(s.audibleSampleLocked()), s.opts.SampleRate
}
func (s *Sink) setDisplayParameter(ctx context.Context, ctype string, body []byte) error {
	cl, uri, cur, _ := s.metadataTarget()
	if cl == nil {
		return nil
	}
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := cl.Do(cctx, "SET_PARAMETER", uri, map[string]string{"Content-Type": ctype, "RTP-Info": "rtptime=" + uitoa(uint64(cur))}, body)
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
