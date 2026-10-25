package raop

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"songloft/internal/player/sink"
)

// Options 建 sink 的参数。
type Options struct {
	// Address 设备 RTSP 地址 host:port。
	Address string
	// DeviceID 设备 MAC（mDNS 服务名里的那串），用于拼 RTSP URI。
	DeviceID string
	// PublicKey 设备 TXT 记录里的 pk；为空表示设备不支持加密，走明文 L16。
	PublicKey string
	// Encrypted 强制加密。PublicKey 为空时该字段被忽略。
	Encrypted bool
	// SampleRate / Channels 协商后的输出格式。
	SampleRate int
	Channels   int
	// Latency 起播提前量，RAOP 默认 2s。
	Latency time.Duration
	// DialTimeout 建连超时。
	DialTimeout time.Duration
}

// Sink RAOP 输出端（AirPlay 1 / RAOP 协议）。
type Sink struct {
	id     string
	opts   Options
	caps   sink.Caps
	events chan sink.Event

	mu        sync.Mutex
	rtsp      *Client
	audio     *net.UDPConn
	control   *net.UDPConn
	timing    *net.UDPConn
	pkt       Packetizer
	anchor    int64
	rtpBase   uint32
	rtpAtPlay uint32
	started   bool
	startAt   time.Time
	closed    bool
	volume    float64
	sent      int64 // 已发送采样数，进度上报用
	lastSync  time.Time
	// 设备时钟偏移（设备时间 - 本机时间），由 timing 端口实测；用于同步校正环。
	clockOff  time.Duration
	clockAt   time.Time
	clockSeen bool

	aesKey []byte
	aesIV  []byte
	enc    bool
	// dataAddr / ctrlAddr 设备侧接收端口，来自 SETUP 的 Transport 响应。
	dataAddr *net.UDPAddr
	ctrlAddr *net.UDPAddr
	// pendingAESKey ANNOUNCE 里要声明的 RSA 密文（空表示明文）。
	pendingAESKey string
}

// New 创建 sink。真正建连在 Open。
func New(id string, opts Options) *Sink {
	if opts.SampleRate == 0 {
		opts.SampleRate = 44100
	}
	if opts.Channels == 0 {
		opts.Channels = 2
	}
	if opts.Latency == 0 {
		opts.Latency = 2 * time.Second
	}
	caps := sink.Caps{
		Codecs:         []sink.Codec{sink.CodecL16},
		MaxSampleRate:  opts.SampleRate,
		Channels:       opts.Channels,
		BitDepth:       16,
		SupportsVolume: true,
		SupportsSeek:   true,
		LatencyMs:      int(opts.Latency / time.Millisecond),
		SyncTier:       sink.SyncExact,
	}
	return &Sink{
		id:     id,
		opts:   opts,
		caps:   caps,
		events: make(chan sink.Event, 16),
		volume: 1,
	}
}

func (s *Sink) ID() string                { return s.id }
func (s *Sink) Kind() sink.Kind           { return sink.KindRAOP }
func (s *Sink) Caps() sink.Caps           { return s.caps }
func (s *Sink) Events() <-chan sink.Event { return s.events }

// Open 完成 RTSP 握手：OPTIONS → ANNOUNCE → SETUP → RECORD。
func (s *Sink) Open(ctx context.Context, plan sink.StreamPlan) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("raop: sink 已关闭")
	}
	if s.rtsp != nil {
		return errors.New("raop: 已经 Open")
	}
	if plan.Format.SampleRate > 0 {
		s.opts.SampleRate = plan.Format.SampleRate
	}
	if plan.Format.Channels > 0 {
		s.opts.Channels = plan.Format.Channels
	}
	s.anchor = plan.Position.Milliseconds() * int64(s.opts.SampleRate) / 1000
	s.rtpBase = randomUint32()
	s.pkt.SSRC = randomUint32()

	// 加密只在设备给了公钥时启用。
	if s.opts.Encrypted && s.opts.PublicKey != "" {
		pub, err := ParsePublicKey(s.opts.PublicKey)
		if err != nil {
			return err
		}
		key, iv, err := NewAESKey()
		if err != nil {
			return err
		}
		enc, err := EncryptAESKey(pub, key)
		if err != nil {
			return err
		}
		s.aesKey, s.aesIV, s.enc = key, iv, true
		s.pendingAESKey = enc
	} else {
		s.pendingAESKey = ""
	}

	addr := s.opts.Address
	if addr == "" {
		return errors.New("raop: 缺少设备地址")
	}
	cl, err := Dial(ctx, addr, s.opts.DialTimeout)
	if err != nil {
		return err
	}
	s.rtsp = cl
	uri := s.uri()

	if resp, err := cl.Do(ctx, "OPTIONS", "*", nil, nil); err != nil {
		s.closeConnsLocked()
		return err
	} else if resp.Status != 200 {
		s.closeConnsLocked()
		return fmt.Errorf("raop: OPTIONS 返回 %d", resp.Status)
	}

	sdp := BuildSDP(SDP{
		Origin:     hostOf(addr),
		SessionID:  randomUint32(),
		SampleRate: s.opts.SampleRate,
		Channels:   s.opts.Channels,
		AESKey:     s.pendingAESKey,
		AESIV:      EncodeIV(s.aesIV),
	})
	if resp, err := cl.Do(ctx, "ANNOUNCE", uri, map[string]string{
		"Content-Type": "application/sdp",
	}, sdp); err != nil {
		s.closeConnsLocked()
		return err
	} else if resp.Status != 200 {
		s.closeConnsLocked()
		return fmt.Errorf("raop: ANNOUNCE 返回 %d", resp.Status)
	}

	audio, control, timing, err := listenUDPs()
	if err != nil {
		s.closeConnsLocked()
		return err
	}
	s.audio, s.control, s.timing = audio, control, timing
	transport := fmt.Sprintf("RTP/AVP/UDP;unicast;interleaved=0-1;mode=record;control_port=%d;timing_port=%d",
		localPort(control), localPort(timing))
	resp, err := cl.Do(ctx, "SETUP", uri, map[string]string{"Transport": transport}, nil)
	if err != nil {
		s.closeConnsLocked()
		return err
	}
	if resp.Status != 200 {
		s.closeConnsLocked()
		return fmt.Errorf("raop: SETUP 返回 %d", resp.Status)
	}
	// 设备在 SETUP 响应里告诉自己收流的端口；缺省按协议默认 6000/6001。
	serverPort, controlPort := ParseTransportPorts(resp.HeaderValue("Transport"))
	ip := net.ParseIP(hostOf(addr))
	s.dataAddr = &net.UDPAddr{IP: ip, Port: serverPort}
	s.ctrlAddr = &net.UDPAddr{IP: ip, Port: controlPort}

	if err := s.recordLocked(ctx); err != nil {
		s.closeConnsLocked()
		return err
	}
	s.startTimingReader(timing)
	return nil
}

// Start 开始发声。at 是组内统一时间轴上的起播时刻。
func (s *Sink) Start(at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rtsp == nil {
		return errors.New("raop: 未 Open")
	}
	s.started = true
	s.startAt = at
	s.rtpAtPlay = s.tsLocked(s.anchor)
	if err := s.sendSyncLocked(); err != nil {
		return err
	}
	s.emit(sink.Event{Type: sink.EventPlaying})
	return nil
}

// startTimingReader 读设备发来的 timing 包，估计设备时钟相对本机的偏移。
//
// RAOP 设备会周期性向我们的 timing_port 发 NTP 请求（origin=它的发送时刻），
// 收到时刻与本机时钟之差就是偏移的粗估（含单向网络时延，局域网内 <1ms）。
// 没有这个通道就只能假设设备时钟与本机一致，同步校正环也就无从判断漂移。
func (s *Sink) startTimingReader(conn *net.UDPConn) {
	if conn == nil {
		return
	}
	go func() {
		buf := make([]byte, 256)
		for {
			n, _, err := conn.ReadFromUDP(buf)
			if err != nil {
				return // 连接已关闭
			}
			if n < 12 || buf[1] != ControlTiming {
				continue
			}
			recv := time.Now()
			sec := binary.BigEndian.Uint32(buf[4:8])
			frac := binary.BigEndian.Uint32(buf[8:12])
			origin := time.Unix(int64(sec)-NTPEpochOffset, int64(float64(frac)/4294967296.0*1e9))
			off := origin.Sub(recv)
			s.mu.Lock()
			if !s.closed {
				s.clockOff = off
				s.clockAt = recv
				s.clockSeen = true
			}
			s.mu.Unlock()
		}
	}()
}

// PlaybackPosition 实现 sink.PositionReporter。
//
// RAOP 没有应用层回传，位置靠"设备时钟 + 起播时刻"推算：设备实际按自己的
// 时钟发声，晶振偏快就会比时间轴跑得快。只有最近 10s 内测到过时钟偏移才可信。
func (s *Sink) PlaybackPosition() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || !s.clockSeen || s.startAt.IsZero() {
		return 0, false
	}
	if time.Since(s.clockAt) > 10*time.Second {
		return 0, false
	}
	deviceNow := time.Now().Add(s.clockOff)
	base := time.Duration(s.anchor) * time.Second / time.Duration(s.opts.SampleRate)
	pos := base + deviceNow.Sub(s.startAt)
	if pos < 0 {
		pos = 0
	}
	return pos, true
}

// WriteFrame 加密（可选）后发一个 RTP 包。
func (s *Sink) WriteFrame(f sink.Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.audio == nil {
		return errors.New("raop: 未 Open")
	}
	payload := f.Data
	if s.enc {
		var err error
		payload, err = EncryptFrames(s.aesKey, s.aesIV, f.Data)
		if err != nil {
			return err
		}
	}
	ts := s.tsLocked(f.SampleIndex)
	frameBytes := s.opts.Channels * 2
	samples := len(f.Data) / frameBytes
	marker := s.sent == 0
	if _, err := s.audio.WriteToUDP(BuildRTPPacket(s.pkt.Seq, ts, s.pkt.SSRC, marker, payload), s.dataAddr); err != nil {
		return fmt.Errorf("raop: 发送音频包失败: %w", err)
	}
	s.pkt.Seq++
	s.sent += int64(samples)
	if s.lastSync.IsZero() || time.Since(s.lastSync) >= time.Second {
		s.lastSync = time.Now()
		if err := s.sendSyncLocked(); err != nil {
			return err
		}
	}
	return nil
}

// Seek 中断当前流并跳转到指定采样。
func (s *Sink) Seek(sample uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rtsp == nil {
		return errors.New("raop: 未 Open")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.rtsp.Do(ctx, "FLUSH", s.uri(), map[string]string{
		"RTP-Info": fmt.Sprintf("seq=%d;rtptime=%d", s.pkt.Seq, s.tsLocked(int64(sample))),
	}, nil); err != nil {
		return err
	}
	s.anchor = int64(sample)
	s.rtpBase = randomUint32()
	s.sent = 0
	s.rtpAtPlay = s.tsLocked(s.anchor)
	return s.recordLocked(ctx)
}

// SetVolume 设音量（0~1 映射到 -144~0 dB）。
func (s *Sink) SetVolume(v float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	s.volume = v
	if s.rtsp == nil {
		return nil
	}
	db := -144.0
	if v > 0 {
		db = 20 * math.Log10(v)
		if db < -144 {
			db = -144
		}
	}
	body := []byte(fmt.Sprintf("volume: %.2f\r\n", db))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := s.rtsp.Do(ctx, "SET_PARAMETER", s.uri(), map[string]string{
		"Content-Type": "text/parameters",
	}, body)
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return fmt.Errorf("raop: SET_PARAMETER volume 返回 %d", resp.Status)
	}
	return nil
}

// Stop 结束会话。
func (s *Sink) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rtsp == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.rtsp.Do(ctx, "TEARDOWN", s.uri(), nil, nil)
	s.started = false
	s.emit(sink.Event{Type: sink.EventStopped})
	return err
}

// Close 释放所有连接。
func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.closeConnsLocked()
	close(s.events)
	return nil
}

// --- 内部 ---

func (s *Sink) closeConnsLocked() {
	if s.rtsp != nil {
		_ = s.rtsp.Close()
		s.rtsp = nil
	}
	for _, c := range []*net.UDPConn{s.audio, s.control, s.timing} {
		if c != nil {
			_ = c.Close()
		}
	}
	s.audio, s.control, s.timing = nil, nil, nil
}

func (s *Sink) recordLocked(ctx context.Context) error {
	resp, err := s.rtsp.Do(ctx, "RECORD", s.uri(), map[string]string{
		"Range":    "npt=0-",
		"RTP-Info": fmt.Sprintf("seq=%d;rtptime=%d", s.pkt.Seq, s.tsLocked(s.anchor)),
	}, nil)
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return fmt.Errorf("raop: RECORD 返回 %d", resp.Status)
	}
	return nil
}

func (s *Sink) sendSyncLocked() error {
	if s.control == nil {
		return nil
	}
	pkt := BuildSyncPacket(s.tsLocked(s.anchor)+uint32(s.sent), s.rtpAtPlay, ToNTP(time.Now()))
	if s.ctrlAddr == nil {
		return nil
	}
	_, err := s.control.WriteToUDP(pkt, s.ctrlAddr)
	return err
}

// tsLocked 采样序号 → RTP 时间戳。anchor 之后按采样差推进。
func (s *Sink) tsLocked(sample int64) uint32 {
	return s.rtpBase + uint32(sample-s.anchor)
}

func (s *Sink) uri() string {
	id := s.opts.DeviceID
	if id == "" {
		id = "0"
	}
	return fmt.Sprintf("rtsp://%s/%s", s.opts.Address, id)
}

func (s *Sink) emit(ev sink.Event) {
	select {
	case s.events <- ev:
	default:
	}
}

func listenUDPs() (audio, control, timing *net.UDPConn, err error) {
	if audio, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero}); err != nil {
		return nil, nil, nil, err
	}
	if control, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero}); err != nil {
		audio.Close()
		return nil, nil, nil, err
	}
	if timing, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4zero}); err != nil {
		audio.Close()
		control.Close()
		return nil, nil, nil, err
	}
	return audio, control, timing, nil
}

func localPort(c *net.UDPConn) int {
	if c == nil {
		return 0
	}
	return c.LocalAddr().(*net.UDPAddr).Port
}

// ParseTransportPorts 从 SETUP 响应的 Transport 头里取设备端口。
//
// 形如 `RTP/AVP/UDP;unicast;server_port=6000;control_port=6001;timing_port=6002`。
// 缺省值来自 RAOP 规范，老设备（AirPort Express）经常不返回这一段。
func ParseTransportPorts(transport string) (server, control int) {
	server, control = 6000, 6001
	for _, part := range strings.Split(transport, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n <= 0 {
			continue
		}
		switch strings.TrimSpace(k) {
		case "server_port":
			server = n
		case "control_port":
			control = n
		}
	}
	return server, control
}

func hostOf(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

func randomUint32() uint32 {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uint32(time.Now().UnixNano())
	}
	return binary.BigEndian.Uint32(b[:])
}

// ParseTXT 解析 mDNS TXT 记录里的能力字段（ch/cn/sr/ss/pw/pk/tp）。
//
// 返回值给发现逻辑用：pw=true 的设备第一版直接跳过；pk 是加密公钥。
func ParseTXT(txt []string) sink.Caps {
	caps := sink.Caps{Codecs: []sink.Codec{sink.CodecL16}, Channels: 2, BitDepth: 16, SyncTier: sink.SyncExact, LatencyMs: 2000}
	for _, kv := range txt {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch k {
		case "ch":
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				caps.Channels = n
			}
		case "sr":
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				caps.MaxSampleRate = n
			}
		case "ss":
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				caps.BitDepth = n
			}
		case "pw":
			caps.NeedsPassword = v == "true"
		case "sf", "md":
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				caps.BitDepth = n
			}
		}
	}
	return caps
}
