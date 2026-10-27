package raop

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
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
	ALAC bool
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
	// OnControl 设备反控回调（DACP：音箱物理按键/遥控）。nil 表示不启用。
	OnControl func(cmd string, value float64)
}

// Sink RAOP 输出端（AirPlay 1 / RAOP 协议）。
type Sink struct {
	id     string
	opts   Options
	caps   sink.Caps
	events chan sink.Event

	mu          sync.Mutex
	rtsp        *Client
	audio       *net.UDPConn
	control     *net.UDPConn
	timing      *net.UDPConn
	pkt         Packetizer
	anchor      int64
	rtpBase     uint32
	sessionID   uint32
	rtpAtPlay   uint32
	started     bool
	startAt     time.Time
	closed      bool
	volume      float64
	sent        int64 // 已发送采样数，进度上报用
	lastSync    time.Time
	sessionDone chan struct{}
	// PCM frames from the distributor are not aligned to RAOP packet boundaries.
	pendingPCM   []byte
	pendingAt    int64
	backlog      [packetBacklogSize]cachedPacket
	resent       uint64
	resendMisses uint64
	// 设备时钟偏移（设备时间 - 本机时间），由 timing 端口实测；用于同步校正环。
	clockOff  time.Duration
	clockAt   time.Time
	clockSeen bool
	clockMu   sync.Mutex
	clockConn *net.UDPConn

	aesKey []byte
	aesIV  []byte
	enc    bool
	// dataAddr / ctrlAddr 设备侧接收端口，来自 SETUP 的 Transport 响应。
	dataAddr *net.UDPAddr
	ctrlAddr *net.UDPAddr
	// pendingAESKey ANNOUNCE 里要声明的 RSA 密文（空表示明文）。
	pendingAESKey string
	// plan 当前曲目信息（gapless 接续时只更新它）。
	plan sink.StreamPlan
	// dacp 反控端点（Open 成功后启动，对齐 Roon 每 AirPlayClient 一个）。
	dacp *DACPServer
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
	s.sent = 0
	s.pendingPCM = nil
	s.backlog = [packetBacklogSize]cachedPacket{}
	s.resent, s.resendMisses = 0, 0
	s.lastSync = time.Time{}
	s.rtpBase = randomUint32()
	s.sessionID = randomUint32()
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
	s.sessionDone = make(chan struct{})
	uri := s.uri()

	if resp, err := cl.Do(ctx, "OPTIONS", "*", nil, nil); err != nil {
		s.abortSessionLocked()
		return err
	} else if resp.Status != 200 {
		s.abortSessionLocked()
		return statusError("OPTIONS", resp.Status)
	}

	sdp := BuildSDP(SDP{
		Origin:      hostOf(cl.conn.LocalAddr().String()),
		Destination: hostOf(addr),
		ALAC:        s.opts.ALAC,
		SessionID:   s.sessionID,
		SampleRate:  s.opts.SampleRate,
		Channels:    s.opts.Channels,
		AESKey:      s.pendingAESKey,
		AESIV:       EncodeIV(s.aesIV),
	})
	if resp, err := cl.Do(ctx, "ANNOUNCE", uri, map[string]string{
		"Content-Type": "application/sdp",
	}, sdp); err != nil {
		s.abortSessionLocked()
		return err
	} else if resp.Status != 200 {
		s.abortSessionLocked()
		return statusError("ANNOUNCE", resp.Status)
	}

	audio, control, timing, err := listenUDPs(net.ParseIP(hostOf(cl.conn.LocalAddr().String())))
	if err != nil {
		s.abortSessionLocked()
		return err
	}
	s.audio, s.control, s.timing = audio, control, timing
	s.startTimingReader(timing)
	transport := fmt.Sprintf("RTP/AVP/UDP;unicast;interleaved=0-1;mode=record;control_port=%d;timing_port=%d",
		localPort(control), localPort(timing))
	resp, err := cl.Do(ctx, "SETUP", uri, map[string]string{"Transport": transport}, nil)
	if err != nil {
		s.abortSessionLocked()
		return err
	}
	if resp.Status != 200 {
		s.abortSessionLocked()
		return statusError("SETUP", resp.Status)
	}
	// 设备在 SETUP 响应里告诉自己收流的端口；缺省按协议默认 6000/6001。
	serverPort, controlPort := ParseTransportPorts(resp.HeaderValue("Transport"))
	slog.Info("AirPlay transport negotiated", "device", s.id, "transport", resp.HeaderValue("Transport"), "local", cl.conn.LocalAddr().String())
	ip := net.ParseIP(hostOf(addr))
	s.dataAddr = &net.UDPAddr{IP: ip, Port: serverPort}
	s.ctrlAddr = &net.UDPAddr{IP: ip, Port: controlPort}
	s.startControlReader(control, ip)

	if err := s.recordLocked(ctx); err != nil {
		s.abortSessionLocked()
		return err
	}
	db := -144.0
	if s.volume > 0 {
		db = 20 * math.Log10(s.volume)
	}
	resp, err = cl.Do(ctx, "SET_PARAMETER", uri, map[string]string{"Content-Type": "text/parameters"}, []byte(fmt.Sprintf("volume: %.2f\r\n", db)))
	if err != nil {
		s.abortSessionLocked()
		return fmt.Errorf("raop: initial volume failed: %w", err)
	}
	if resp.Status != 200 {
		s.abortSessionLocked()
		return statusError("SET_PARAMETER", resp.Status)
	}
	s.startKeepAlive(cl, s.sessionDone, 15*time.Second)

	// DACP 反控端点：RECORD 成功后启动，DACP-ID/Active-Remote 随后续所有
	// 请求携带（对齐 AirPlayClientV1.CreateRequest:2884-2893）。
	if s.opts.OnControl != nil {
		d := NewDACPServer(s.opts.OnControl)
		if err := d.Start(); err != nil {
			slog.Warn("DACP 反控端点启动失败（不影响播放）", "device", s.id, "err", err)
		} else {
			s.dacp = d
			cl.DefaultHeaders["DACP-ID"] = d.ID
			cl.DefaultHeaders["Active-Remote"] = d.ActiveRemote
			slog.Info("DACP 反控已启用", "device", s.id, "dacp_id", d.ID)
		}
	}
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

// Reply to all timing requests independently of the RTSP/Open mutex: receivers
// may wait for several timing exchanges before returning SETUP/RECORD.
func (s *Sink) startTimingReader(conn *net.UDPConn) {
	if conn == nil {
		return
	}
	s.clockMu.Lock()
	s.clockConn, s.clockSeen = conn, false
	s.clockMu.Unlock()
	go func() {
		buf := make([]byte, 256)
		first := true
		for {
			n, peer, err := conn.ReadFromUDP(buf)
			if err != nil {
				return // 连接已关闭
			}
			if first && n >= 2 {
				slog.Info("AirPlay timing received", "device", s.id, "bytes", n, "type", buf[1])
				first = false
			}
			if n < 32 || buf[1] != 0xd2 {
				continue
			}
			recv := time.Now()
			response := make([]byte, 32)
			response[0], response[1] = 0x80, 0xd3
			copy(response[2:4], buf[2:4])
			copy(response[8:16], buf[24:32])
			for i, stamp := range []NTPTime{ToNTP(recv), ToNTP(time.Now())} {
				binary.BigEndian.PutUint32(response[16+i*8:], stamp.Sec)
				binary.BigEndian.PutUint32(response[20+i*8:], stamp.Frac)
			}
			_, _ = conn.WriteToUDP(response, peer)
			s.clockMu.Lock()
			if s.clockConn == conn {
				s.clockOff = 0
				s.clockAt = recv
				s.clockSeen = true
			}
			s.clockMu.Unlock()
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
	s.clockMu.Lock()
	defer s.clockMu.Unlock()
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
	stride := s.opts.Channels * 2
	if len(f.Data)%stride != 0 {
		return errors.New("raop: PCM 帧未对齐采样")
	}
	if len(s.pendingPCM) == 0 || f.SampleIndex != s.pendingAt+int64(len(s.pendingPCM)/stride) {
		// A deliberate seek/skip is a new sample position, not contiguous PCM.
		s.pendingPCM = s.pendingPCM[:0]
		s.pendingAt = f.SampleIndex
	}
	s.pendingPCM = append(s.pendingPCM, f.Data...)
	packetBytes := frameSamples * stride
	for len(s.pendingPCM) >= packetBytes {
		if err := s.writeChunkLocked(sink.Frame{Data: s.pendingPCM[:packetBytes], SampleIndex: s.pendingAt}); err != nil {
			return err
		}
		s.pendingAt += frameSamples
		s.pendingPCM = s.pendingPCM[packetBytes:]
	}
	return nil
}

func (s *Sink) writeChunkLocked(f sink.Frame) error {
	if s.audio == nil {
		return errors.New("raop: 未 Open")
	}
	payload := f.Data
	if s.opts.ALAC {
		payload = encodeALAC(f.Data, s.opts.Channels)
	}
	if s.enc {
		var err error
		payload, err = EncryptFrames(s.aesKey, s.aesIV, payload)
		if err != nil {
			return err
		}
	}
	ts := s.tsLocked(f.SampleIndex)
	frameBytes := s.opts.Channels * 2
	samples := len(f.Data) / frameBytes
	marker := s.sent == 0
	if marker {
		nonzero := 0
		for _, b := range f.Data {
			if b != 0 {
				nonzero++
			}
		}
		slog.Info("AirPlay first audio packet", "device", s.id, "alac", s.opts.ALAC, "samples", samples, "nonzero_bytes", nonzero, "target", s.dataAddr.String())
	}
	packet := BuildRTPPacket(s.pkt.Seq, ts, s.pkt.SSRC, marker, payload)
	if _, err := s.audio.WriteToUDP(packet, s.dataAddr); err != nil {
		return fmt.Errorf("raop: 发送音频包失败: %w", err)
	}
	s.backlog[int(s.pkt.Seq)%packetBacklogSize] = cachedPacket{seq: s.pkt.Seq, data: packet}
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
	s.pendingPCM = nil
	s.backlog = [packetBacklogSize]cachedPacket{}
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
	slog.Info("AirPlay session stopped", "device", s.id, "samples", s.sent, "retransmitted", s.resent, "resend_misses", s.resendMisses)
	s.closeConnsLocked()
	s.started = false
	s.emit(sink.Event{Type: sink.EventStopped})
	return err
}

// Continue 在同一 RTSP/RTP 会话里接续下一曲（gapless）。
//
// 只更新元数据：RTP 时间戳由帧的 SampleIndex 推导，本来就跨曲连续，
// 所以这里既不能重新 ANNOUNCE，也不能重新 Start——重新 Start 会把
// 锚点按新位置重算，设备端听到的是时间戳倒退。
func (s *Sink) Continue(plan sink.StreamPlan) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.rtsp == nil {
		return errors.New("raop: 会话已关闭，无法接续")
	}
	s.plan = plan
	s.emit(sink.Event{Type: sink.EventPlaying, Position: plan.Position})
	return nil
}

// Standby 停发 RTP（TEARDOWN）。
//
// RTSP 协议没有待机指令，AirPlay 音箱不会因此进入硬件待机；这里只保证
// "不再往它推音频"，由上层把它从同步组里摘掉并标 standby。
func (s *Sink) Standby(context.Context) error {
	return s.Stop()
}

// Wake 唤醒：AirPlay 无上电指令，下一次 Open 会重新 ANNOUNCE/SETUP。
func (s *Sink) Wake(context.Context) error { return nil }

// Close 释放所有连接。
func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.abortSessionLocked()
	close(s.events)
	return nil
}

// --- 内部 ---

func statusError(method string, status int) error {
	if status == 453 {
		return fmt.Errorf("raop: %s 返回 453（音箱拒绝新会话，可能仍有上一连接未释放或被其他播放占用）", method)
	}
	return fmt.Errorf("raop: %s 返回 %d", method, status)
}

// Release only the session acknowledged on our own RTSP connection. A failed
// OPTIONS without a session must never tear down another sender's playback.
func (s *Sink) abortSessionLocked() {
	if s.rtsp != nil && s.rtsp.Session() != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _ = s.rtsp.Do(ctx, "TEARDOWN", s.uri(), nil, nil)
		cancel()
	}
	s.closeConnsLocked()
}

func (s *Sink) closeConnsLocked() {
	if s.dacp != nil {
		s.dacp.Close() //nolint:errcheck // 关闭失败无需处理
		s.dacp = nil
	}
	s.clockMu.Lock()
	s.clockConn, s.clockSeen = nil, false
	s.clockMu.Unlock()
	if s.sessionDone != nil {
		close(s.sessionDone)
		s.sessionDone = nil
	}
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
	s.pendingPCM = nil
	s.backlog = [packetBacklogSize]cachedPacket{}
}

// Keep the RTSP session alive without holding the PCM writer's mutex while
// waiting for a control response. Some receivers expire an otherwise active
// UDP stream when no RTSP requests arrive for 30 seconds.
func (s *Sink) startKeepAlive(cl *Client, done <-chan struct{}, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				resp, err := cl.Do(ctx, "OPTIONS", "*", nil, nil)
				cancel()
				if err != nil || resp.Status != 200 {
					slog.Warn("AirPlay keepalive failed", "device", s.id, "err", err)
					return
				}
			}
		}
	}()
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
		return statusError("RECORD", resp.Status)
	}
	return nil
}

func (s *Sink) sendSyncLocked() error {
	if s.control == nil {
		return nil
	}
	now := time.Now()
	playing := s.rtpAtPlay + uint32(int64(now.Sub(s.startAt).Seconds()*float64(s.opts.SampleRate)))
	buffered := playing + uint32(s.opts.Latency.Seconds()*float64(s.opts.SampleRate))
	pkt := BuildSyncPacket(playing, buffered, ToNTP(now))
	if s.sent == 0 {
		pkt[0] = 0x90
	}
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
	id := strconv.FormatUint(uint64(s.sessionID), 10)
	host := s.opts.Address
	if s.rtsp != nil {
		host = hostOf(s.rtsp.conn.LocalAddr().String())
	}
	return fmt.Sprintf("rtsp://%s/%s", host, id)
}

func (s *Sink) emit(ev sink.Event) {
	select {
	case s.events <- ev:
	default:
	}
}

func listenUDPs(localIP net.IP) (audio, control, timing *net.UDPConn, err error) {
	if audio, err = net.ListenUDP("udp4", &net.UDPAddr{IP: localIP}); err != nil {
		return nil, nil, nil, err
	}
	if control, err = net.ListenUDP("udp4", &net.UDPAddr{IP: localIP}); err != nil {
		audio.Close()
		return nil, nil, nil, err
	}
	if timing, err = net.ListenUDP("udp4", &net.UDPAddr{IP: localIP}); err != nil {
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
	caps := sink.Caps{Codecs: []sink.Codec{sink.CodecL16}, Channels: 2, BitDepth: 16, SyncTier: sink.SyncExact, LatencyMs: 2000, SupportsVolume: true, SupportsSeek: true}
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
		}
	}
	return caps
}
