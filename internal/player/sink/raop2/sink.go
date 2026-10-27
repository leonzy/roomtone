package raop2

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
	"songloft/internal/player/sink/airplay"
)

// Options 建 sink 的参数。
type Options struct {
	// Address 设备 RTSP 地址 host:port。
	Address string
	// DeviceID 设备 MAC（mDNS 服务名里的那串），用于拼 RTSP URI。
	DeviceID string
	// PublicKey 设备 TXT 记录里的 pk；为空表示设备不支持加密，走明文 L16。
	PublicKey string

	// SampleRate / Channels 协商后的输出格式。
	SampleRate int
	Channels   int
	// Latency 起播提前量。
	Latency time.Duration
	// DialTimeout 建连超时。
	DialTimeout time.Duration
	// Password 设备密码（HTTP Digest，401 时用）。
	Password string
	// OnControl 设备反控回调（DACP + event 通道音量）。nil 表示不启用。
	OnControl func(cmd string, value float64)

	// UseAirPlay2 V2 模式（对齐 _use_airplay2 判定：GroupId != null /
	// StatusFlags 有 DeviceWasSetupForHKAccessControl / 曾 transient|homekit
	// 配对）。false 走 V1 兼容路径（Announce → Record → Setup）。
	UseAirPlay2 bool
	// RequiresPairing features 声明需要配对（→ pair-setup transient →
	// 加密 RTSP）。
	RequiresPairing bool
	// SupportsBPlistNowPlaying features 位（→ 元数据后补 POST /command
	// updateMRPlaybackState）。
	SupportsBPlistNowPlaying bool
	// Features 原始位域（发现层填，A2P-6）。
	Features uint64
}

// Sink AirPlay 2 输出端（AirPlayClientV2.cs 的 Go 对应物）。
//
// 两条会话路径（对齐 Setup 状态机的 _use_airplay2 分派）：
//
//	V2：OPTIONS →（配对）→ SETUP#1(bplist{timingProtocol:NTP} →
//	     {timingPort,eventPort}，连 event 通道) → SETUP#2(Transport 头 →
//	     UDP 端口) → RECORD。**不发 ANNOUNCE**（反编译证实 ANNOUNCE 仅在
//	     V1 兼容路径）。
//	V1 兼容：OPTIONS →（配对）→ ANNOUNCE(SDP) → RECORD → SETUP(Transport 头)
//	     （对齐 SetupAirPlay1 的反编译顺序——RECORD 先于 SETUP）。
//
// 音频路径两模式共享：RTP+ALAC(352 采样/包) → 加密（ChaChaPoly：AAD=RTP 头
// [4:12]、包尾 8B nonce 尾 / AES：CBC+明文尾 / 明文）→ UDP；timing 0xd2→0xd3
// 应答；sync 包；4000 包重传缓存。
type Sink struct {
	id     string
	opts   Options
	caps   sink.Caps
	events chan sink.Event

	mu        sync.Mutex
	rtsp      *RTSPClient
	pairing   *Pairing
	dacp      *airplay.DACPServer
	v1Compat  bool // V1 兼容路径（Announce→Record→Setup）
	closed    bool
	plan      sink.StreamPlan
	anchor    int64
	rtpBase   uint32
	sessionID uint32
	rtpSeq    uint16
	rtpAtPlay uint32
	started   bool
	startAt   time.Time
	volume    float64
	sent      int64

	// 音频 UDP
	audio *net.UDPConn // 发包 socket
	ctrl  *net.UDPConn // 收 retransmit 请求
	timng *net.UDPConn // 收 timing
	// 设备侧端口
	serverPort  int
	controlPort int
	timingPort  int
	// event 通道（V2）
	eventConn net.Conn
	eventEnc  *EncryptionContext

	// 音频加密
	encType EncryptionType
	aesKey  []byte
	aesIV   []byte
	// sessionKey：ChaChaPoly 音频加密密钥（pair-setup 的 sharedSecret 前 32B）
	sessionKey []byte

	// PCM 缓冲（分发帧不按 352 采样对齐时累积）
	pendingPCM []byte
	pendingAt  int64

	// 重传缓存（V2 为 4000 包，对齐 _retransmit_seqs.Count > 4000）
	backlog      map[uint16][]byte
	backlogOrder []uint16

	// 时钟偏移（同步校正用）
	clockOff  time.Duration
	clockAt   time.Time
	clockSeen bool

	lastSync time.Time
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
		opts.Latency = time.Second
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

func (s *Sink) emit(ev sink.Event) {
	select {
	case s.events <- ev:
	default:
	}
}

// --- 会话建立 ---

func (s *Sink) uri() string {
	id := strconv.FormatUint(uint64(s.sessionID), 10)
	host := s.opts.Address
	if s.rtsp != nil {
		host = hostOf(s.rtsp.Conn().LocalAddr().String())
	}
	return fmt.Sprintf("rtsp://%s/%s", host, id)
}

func hostOf(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// Open 建立会话：OPTIONS →（配对）→ V2 或 V1 兼容路径。
func (s *Sink) Open(ctx context.Context, plan sink.StreamPlan) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("raop2: sink 已关闭")
	}
	if s.rtsp != nil {
		return errors.New("raop2: 已经 Open")
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
	s.backlog = map[uint16][]byte{}
	s.backlogOrder = nil
	s.rtpBase = randomUint32()
	s.sessionID = randomUint32()

	addr := s.opts.Address
	if addr == "" {
		return errors.New("raop2: 缺少设备地址")
	}
	cl, err := Dial(ctx, addr, s.opts.DialTimeout)
	if err != nil {
		return err
	}
	s.rtsp = cl

	// OPTIONS：探测加密类型（Apple-Response → AES）+ digest 认证。
	if err := s.options(ctx); err != nil {
		s.abortLocked()
		return err
	}

	// 配对（features 声明需要）：pair-setup transient → 加密 RTSP。
	if s.opts.RequiresPairing {
		s.pairing = NewPairing(cl, nil)
		if err := s.pairing.PairSetup(ctx, s.opts.Password, true); err != nil {
			s.abortLocked()
			return fmt.Errorf("raop2: pair-setup: %w", err)
		}
		cl.InitEncryption(EncryptionChaChaPoly, s.pairing.SharedSecret())
		// 音频 ChaChaPoly 密钥 = sharedSecret 前 32B（对齐 _session_key）。
		s.encType = EncryptionChaChaPoly
		s.sessionKey = append([]byte(nil), s.pairing.SharedSecret()[:32]...)
	}

	// 会话路径
	if s.opts.UseAirPlay2 {
		if err := s.setupAirPlay2(ctx); err != nil {
			s.abortLocked()
			return err
		}
	} else {
		if err := s.setupAirPlay1(ctx); err != nil {
			s.abortLocked()
			return err
		}
	}

	// DACP 反控（复用 airplay 共享件）。
	if s.opts.OnControl != nil {
		d := airplay.NewDACPServer(s.opts.OnControl)
		if err := d.Start(); err != nil {
			slog.Warn("DACP 反控端点启动失败（不影响播放）", "device", s.id, "err", err)
		} else {
			s.dacp = d
			cl.DefaultHeaders["DACP-ID"] = d.ID
			cl.DefaultHeaders["Active-Remote"] = d.ActiveRemote
		}
	}
	return nil
}

// options OPTIONS 探测：Apple-Response → AES；401 → digest（复用 airplay）。
func (s *Sink) options(ctx context.Context) error {
	resp, err := s.doAuth(ctx, "OPTIONS", "*", nil, nil)
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return fmt.Errorf("raop2: OPTIONS 返回 %d", resp.Status)
	}
	if resp.HeaderValue("Apple-Response") != "" {
		s.encType = EncryptionAES
	}
	return nil
}

// doAuth 401 + 密码 → digest 重试一次（复用 airplay.ComputeDigest）。
func (s *Sink) doAuth(ctx context.Context, method, uri string, headers map[string]string, body []byte) (*Response, error) {
	resp, err := s.rtsp.Do(ctx, method, uri, headers, body)
	if err != nil || resp.Status != 401 {
		return resp, err
	}
	if s.opts.Password == "" {
		return nil, errors.New("raop2: 设备要求密码（401），请在该设备的设置里填写密码")
	}
	if s.rtsp.authorization != "" {
		return nil, errors.New("raop2: 设备拒绝授权（401），请检查密码")
	}
	www := resp.HeaderValue("WWW-Authenticate")
	realm, nonce, ok := airplay.ParseWWWAuthenticate(www)
	if !ok {
		return nil, fmt.Errorf("raop2: 401 响应缺少 realm 的 WWW-Authenticate: %q", www)
	}
	s.rtsp.authorization = airplay.AuthorizationHeader(realm, nonce, "*", airplay.ComputeDigest(realm, s.opts.Password, "OPTIONS", "*", nonce))
	return s.rtsp.Do(ctx, method, uri, headers, body)
}

// setupAirPlay2 V2 路径：SETUP#1(bplist) → event → SETUP#2(Transport) → RECORD。
// 不发 ANNOUNCE（对齐反编译：ANNOUNCE 仅在 V1 兼容路径）。
func (s *Sink) setupAirPlay2(ctx context.Context) error {
	// --- SETUP#1（session）：bplist{timingProtocol:"NTP"} ---
	setupBody, err := airplay.EncodeBplist(map[string]any{"timingProtocol": "NTP"})
	if err != nil {
		return err
	}
	resp, err := s.doAuth(ctx, "SETUP", s.uri(), map[string]string{
		"Content-Type": "application/x-apple-binary-plist",
	}, setupBody)
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return fmt.Errorf("raop2: SETUP#1 返回 %d", resp.Status)
	}
	if trans := resp.HeaderValue("Transport"); trans != "" {
		// 设备回了 RTSP Transport 头 → 回退 V1 兼容模式（对齐
		// SetupAirPlay2Session 的 "SETUP was successful in airplay 1 mode"）。
		s.v1Compat = true
		slog.Info("AirPlay2 SETUP#1 回退 V1 模式", "device", s.id, "transport", trans)
		return s.setupAirPlay1AfterSession(ctx, trans)
	}
	// bplist 响应：{timingPort, eventPort}
	fields, err := airplay.DecodeBplistNumbers(resp.Body)
	if err != nil {
		return fmt.Errorf("raop2: SETUP#1: %w", err)
	}
	s.timingPort = fields["timingPort"]
	eventPort := fields["eventPort"]
	if s.timingPort == 0 || eventPort == 0 {
		return fmt.Errorf("raop2: SETUP#1 响应缺少 timingPort/eventPort: %v", fields)
	}

	// --- event 通道：TCP 连 eventPort（Events 密钥交叉）---
	if err := s.connectEvent(ctx, eventPort); err != nil {
		return err
	}

	// --- SETUP#2（audio）：Transport 头协商 UDP 端口 ---
	if err := s.setupStream(ctx); err != nil {
		return err
	}

	// --- RECORD ---
	return s.record(ctx)
}

// setupStream SETUP(audio)：Transport 头 → server/control/timing 端口。
func (s *Sink) setupStream(ctx context.Context) error {
	if err := s.listenUDPs(); err != nil {
		return err
	}
	transport := fmt.Sprintf("RTP/AVP/UDP;unicast;interleaved=0-1;mode=record;control_port=%d;timing_port=%d",
		localPort(s.ctrl), localPort(s.timng))
	resp, err := s.doAuth(ctx, "SETUP", s.uri(), map[string]string{"Transport": transport}, nil)
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return fmt.Errorf("raop2: SETUP(audio) 返回 %d", resp.Status)
	}
	sp, cp, tp := parseTransportPorts(resp.HeaderValue("Transport"))
	if sp == 0 {
		return fmt.Errorf("raop2: SETUP(audio) 响应缺少端口: %q", resp.HeaderValue("Transport"))
	}
	s.serverPort, s.controlPort, s.timingPort = sp, cp, tp
	s.startTimingReader()
	s.startControlReader()
	return nil
}

// setupAirPlay1 V1 兼容路径：ANNOUNCE → RECORD → SETUP（对齐 SetupAirPlay1
// 的反编译顺序——RECORD 先于 SETUP）。
func (s *Sink) setupAirPlay1(ctx context.Context) error {
	s.v1Compat = true
	if err := s.announce(ctx); err != nil {
		return err
	}
	if err := s.record(ctx); err != nil {
		return err
	}
	return s.setupStream(ctx)
}

// setupAirPlay1AfterSession V2 SETUP#1 回退（"SETUP was successful in
// airplay 1 mode"）：Transport 头已拿到端口，剩余 SETUP#2(audio) → RECORD
// （对齐 SetupAirPlay2 的 Session → Stream → Record 顺序；无 ANNOUNCE）。
func (s *Sink) setupAirPlay1AfterSession(ctx context.Context, transport string) error {
	sp, cp, tp := parseTransportPorts(transport)
	s.serverPort, s.controlPort, s.timingPort = sp, cp, tp
	if err := s.setupStream(ctx); err != nil {
		return err
	}
	return s.record(ctx)
}

// announce ANNOUNCE（SDP：ALAC rtpmap/fmtp/min-latency；AES 时 rsaaeskey/aesiv）。
func (s *Sink) announce(ctx context.Context) error {
	host := hostOf(s.rtsp.Conn().LocalAddr().String())
	dev := hostOf(s.opts.Address)
	var b strings.Builder
	fmt.Fprintf(&b, "v=0\r\n")
	fmt.Fprintf(&b, "o=Roon %d %d IN IP4 %s\r\n", s.sessionID, s.sessionID, host)
	fmt.Fprintf(&b, "s=Roon\r\n")
	fmt.Fprintf(&b, "c=IN IP4 %s\r\n", dev)
	fmt.Fprintf(&b, "t=0 0\r\n")
	fmt.Fprintf(&b, "m=audio 0 RTP/AVP 96\r\n")
	fmt.Fprintf(&b, "a=rtpmap:96 AppleLossless\r\n")
	fmt.Fprintf(&b, "a=fmtp:96 352 0 16 40 10 14 2 255 0 0 %d\r\n", s.opts.SampleRate)
	fmt.Fprintf(&b, "a=min-latency:%d\r\n", s.opts.SampleRate/4)
	if s.encType == EncryptionAES {
		// 逐字复制自 AirPlayClientV2.cs Announce() 的硬编码值（legacy 密钥）。
		fmt.Fprintf(&b, "a=rsaaeskey:VjVbxWcmYgbBbhwBNlCh3K0CMNtWoB844BuiHGUJT51zQS7SDpMnlbBIobsKbfEJ3SCgWHRXjYWf7VQWRYtEcfx7ejA8xDIk5PSBYTvXP5dU2QoGrSBv0leDS6uxlEWuxBq3lIxCxpWO2YswHYKJBt06Uz9P2Fq2hDUwl3qOQ8oXb0OateTKtfXEwHJMprkhsJsGDrIc5W5NJFMAo6zCiM9bGSDeH2nvTlyW6bfI/Q0v0cDGUNeY3ut6fsoafRkfpCwYId+bg3diJh+uzw5htHDyZ2sN+BFYHzEfo8iv4KDxzeya9llqg6fRNQ8d5YjpvTnoeEQ9ye9ivjkBjcAfVw\r\n")
		fmt.Fprintf(&b, "a=aesiv:ePRBLI0XN5ArFaaz7ncNZw\r\n")
	}
	fmt.Fprintf(&b, "\r\n")
	resp, err := s.doAuth(ctx, "ANNOUNCE", s.uri(), map[string]string{
		"Content-Type": "application/sdp",
	}, []byte(b.String()))
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return fmt.Errorf("raop2: ANNOUNCE 返回 %d", resp.Status)
	}
	return nil
}

// record RECORD（RTP-Info seq/rtptime + Range npt=0-）。
func (s *Sink) record(ctx context.Context) error {
	resp, err := s.doAuth(ctx, "RECORD", s.uri(), map[string]string{
		"RTP-Info": fmt.Sprintf("seq=%d;rtptime=%d", s.rtpSeq, s.rtpBase),
		"Range":    "npt=0-",
	}, nil)
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return fmt.Errorf("raop2: RECORD 返回 %d", resp.Status)
	}
	if al := resp.HeaderValue("Audio-Latency"); al != "" {
		if v, err := strconv.Atoi(al); err == nil {
			s.caps.LatencyMs = v
		}
	}
	return nil
}

// --- 音频 UDP 设施 ---

func (s *Sink) listenUDPs() error {
	ip := net.ParseIP(hostOf(s.rtsp.Conn().LocalAddr().String()))
	if ip == nil {
		ip = net.IPv4zero
	}
	audio, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
	if err != nil {
		return err
	}
	s.audio = audio
	ctrl, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
	if err != nil {
		audio.Close()
		return err
	}
	s.ctrl = ctrl
	timng, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
	if err != nil {
		audio.Close()
		ctrl.Close()
		return err
	}
	s.timng = timng
	return nil
}

func localPort(conn *net.UDPConn) int {
	return conn.LocalAddr().(*net.UDPAddr).Port
}

// parseTransportPorts 从 Transport 头取 server/control/timing 端口。
func parseTransportPorts(trans string) (server, control, timing int) {
	for _, part := range strings.Split(trans, ";") {
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			continue
		}
		switch strings.TrimSpace(k) {
		case "server_port":
			server = n
		case "control_port":
			control = n
		case "timing_port":
			timing = n
		}
	}
	return
}

// startTimingReader timing 0xd2 → 0xd3 应答（复用 airplay 包的包构造）。
func (s *Sink) startTimingReader() {
	conn := s.timng // 捕获局部引用：Close 置 nil 后协程仍可安全退出
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n < 32 || buf[0]>>6 != 2 || buf[1] != 0xd2 {
				continue
			}
			received := airplay.ToNTP(time.Now())
			resp := airplay.BuildTimingPacket(
				airplay.NTPTime{Sec: binary.BigEndian.Uint32(buf[24:28]), Frac: binary.BigEndian.Uint32(buf[28:32])},
				received, airplay.ToNTP(time.Now()))
			copy(resp[2:4], buf[2:4])
			_, _ = conn.WriteToUDP(resp, from)
		}
	}()
}

// startControlReader 收 retransmit 请求（0xd5）并从缓存重发。
func (s *Sink) startControlReader() {
	conn := s.ctrl
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			receiver := net.ParseIP(hostOf(s.opts.Address))
			if n != 8 || buf[0]>>6 != 2 || buf[1]&0x7f != 0x55 || (receiver != nil && !from.IP.Equal(receiver)) {
				continue
			}
			first, count := binary.BigEndian.Uint16(buf[4:6]), int(binary.BigEndian.Uint16(buf[6:8]))
			if count < 1 || count > 4000 {
				continue
			}
			s.mu.Lock()
			if s.ctrl != conn {
				s.mu.Unlock()
				return
			}
			for i := 0; i < count; i++ {
				seq := first + uint16(i)
				if pkt, ok := s.backlog[seq]; ok {
					reply := make([]byte, 4+len(pkt))
					reply[0], reply[1] = 0x80, 0xd6
					binary.BigEndian.PutUint16(reply[2:4], seq)
					copy(reply[4:], pkt)
					_, _ = conn.WriteToUDP(reply, from)
				}
			}
			s.mu.Unlock()
		}
	}()
}

// --- 播放控制 ---

func (s *Sink) Start(at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startAt = at
	s.started = true
	s.rtpAtPlay = s.rtpBase + uint32(s.anchor)
	return nil
}

// WriteFrame 推帧：累积到 352 采样/包，ALAC 编码 + 加密 + RTP 头 → UDP。
func (s *Sink) WriteFrame(f sink.Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.audio == nil {
		return errors.New("raop2: 会话未建立")
	}
	frameBytes := 4 // s16le 立体声一采样 4 字节
	if s.pendingPCM == nil {
		s.pendingPCM = make([]byte, 0, 352*frameBytes)
		s.pendingAt = f.SampleIndex
	}
	s.pendingPCM = append(s.pendingPCM, f.Data...)
	for len(s.pendingPCM) >= 352*frameBytes {
		pkt := s.pendingPCM[:352*frameBytes]
		if err := s.writeChunkLocked(s.pendingAt, pkt); err != nil {
			return err
		}
		s.pendingPCM = s.pendingPCM[352*frameBytes:]
		s.pendingAt += 352
	}
	if len(s.pendingPCM) == 0 {
		s.pendingPCM = nil
	}
	return nil
}

// writeChunkLocked 一个 352 采样包：ALAC → 加密 → RTP → UDP → 重传缓存。
func (s *Sink) writeChunkLocked(sampleIndex int64, pcm []byte) error {
	alac := airplay.EncodeALAC(pcm, s.opts.Channels)
	rtpTime := s.rtpBase + uint32(sampleIndex)
	s.rtpSeq++

	// RTP 头（12B）：V=2、PT=0x60、seq、rtptime、ssrc
	var head [12]byte
	head[0] = 0x80
	head[1] = 0x60
	binary.BigEndian.PutUint16(head[2:4], s.rtpSeq)
	binary.BigEndian.PutUint32(head[4:8], rtpTime)
	binary.BigEndian.PutUint32(head[8:12], s.rtpBase)

	var pkt []byte
	switch s.encType {
	case EncryptionChaChaPoly:
		// [12B 头][ct][16B tag][8B nonce 尾]，AAD = 头 [4:12]
		pkt = make([]byte, 12+len(alac)+16+8)
		copy(pkt[:12], head[:])
		nonce := make([]byte, 12)
		if _, err := rand.Read(nonce[4:12]); err != nil {
			return err
		}
		ct, err := chachaSealAAD(s.sessionKey, nonce, alac, head[4:12])
		if err != nil {
			return err
		}
		copy(pkt[12:], ct)
		copy(pkt[len(pkt)-8:], nonce[4:12])
	case EncryptionAES:
		// [12B 头][AES-CBC(alac) + 明文尾]
		enc, err := airplay.EncryptFrames(s.aesKey, s.aesIV, alac)
		if err != nil {
			return err
		}
		pkt = make([]byte, 12+len(enc))
		copy(pkt[:12], head[:])
		copy(pkt[12:], enc)
	default:
		pkt = make([]byte, 12+len(alac))
		copy(pkt[:12], head[:])
		copy(pkt[12:], alac)
	}

	// 重传缓存（4000 包）
	s.backlog[s.rtpSeq] = append([]byte(nil), pkt...)
	s.backlogOrder = append(s.backlogOrder, s.rtpSeq)
	if len(s.backlogOrder) > 4000 {
		delete(s.backlog, s.backlogOrder[0])
		s.backlogOrder = s.backlogOrder[1:]
	}

	// sync 包（>1s 间隔）
	if s.lastSync.IsZero() || time.Since(s.lastSync) > time.Second {
		s.sendSyncLocked(rtpTime)
	}

	dst := &net.UDPAddr{IP: net.ParseIP(hostOf(s.opts.Address)), Port: s.serverPort}
	_, err := s.audio.WriteTo(pkt, dst)
	s.sent += int64(len(pkt))
	return err
}

// sendSyncLocked 同步包（0x80 0xd4 0x0007 + rtpNow + NTP + rtpAtPlay）。
func (s *Sink) sendSyncLocked(_ uint32) {
	s.lastSync = time.Now()
	playing := s.rtpBase + uint32(s.audibleSampleLocked())
	buffered := playing + uint32(s.opts.Latency.Seconds()*float64(s.opts.SampleRate))
	pkt := airplay.BuildSyncPacket(playing, buffered, airplay.ToNTP(time.Now()))
	if s.sent == 0 {
		pkt[0] = 0x90
	}
	if s.ctrl == nil || s.controlPort == 0 {
		return
	}
	dst := &net.UDPAddr{IP: net.ParseIP(hostOf(s.opts.Address)), Port: s.controlPort}
	_, _ = s.ctrl.WriteToUDP(pkt, dst)
}

func (s *Sink) Seek(sample uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.doAuth(ctx, "FLUSH", s.uri(), map[string]string{
		"RTP-Info": fmt.Sprintf("seq=%d;rtptime=%d", s.rtpSeq, s.rtpBase+uint32(sample)),
	}, nil)
	if err == nil {
		s.anchor = int64(sample)
		s.rtpAtPlay = s.rtpBase + uint32(sample)
		s.pendingPCM = nil
	}
	return err
}

func (s *Sink) SetVolume(v float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.volume = v
	if s.rtsp == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db := -144.0
	if v > 0 {
		db = 20 * math.Log10(v)
	}
	resp, err := s.rtsp.Do(ctx, "SET_PARAMETER", s.uri(), map[string]string{
		"Content-Type": "text/parameters",
	}, []byte(fmt.Sprintf("volume: %.2f\r\n", db)))
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return fmt.Errorf("raop2: SET_PARAMETER(volume) 返回 %d", resp.Status)
	}
	return nil
}

func (s *Sink) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rtsp == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.rtsp.Do(ctx, "TEARDOWN", s.uri(), nil, nil)
	s.closeConnsLocked()
	s.started = false
	s.emit(sink.Event{Type: sink.EventStopped})
	return err
}

func (s *Sink) Continue(plan sink.StreamPlan) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.rtsp == nil {
		return errors.New("raop2: 会话已关闭，无法接续")
	}
	s.plan = plan
	s.emit(sink.Event{Type: sink.EventPlaying, Position: plan.Position})
	return nil
}

func (s *Sink) Standby(context.Context) error { return s.Stop() }

func (s *Sink) Wake(context.Context) error { return nil }

func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.abortLocked()
	close(s.events)
	return nil
}

func (s *Sink) abortLocked() {
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
	if s.eventConn != nil {
		s.eventConn.Close() //nolint:errcheck // 同上
		s.eventConn = nil
	}
	for _, c := range []*net.UDPConn{s.audio, s.ctrl, s.timng} {
		if c != nil {
			c.Close() //nolint:errcheck // 同上
		}
	}
	s.audio, s.ctrl, s.timng = nil, nil, nil
	if s.rtsp != nil {
		_ = s.rtsp.Close()
	}
}

// PlaybackPosition 实现 sink.PositionReporter。
func (s *Sink) PlaybackPosition() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.startAt.IsZero() {
		return 0, false
	}
	base := time.Duration(s.anchor) * time.Second / time.Duration(s.opts.SampleRate)
	pos := base + time.Since(s.startAt)
	if pos < 0 {
		pos = 0
	}
	return pos, true
}

// --- 元数据（DMAP 同 V1 + updateMRPlaybackState，对齐 AirPlayClientV2.cs:7168-7240）---

func (s *Sink) audibleSampleLocked() int64 {
	if s.startAt.IsZero() {
		return s.anchor
	}
	return s.anchor + int64(time.Since(s.startAt).Seconds()*float64(s.opts.SampleRate))
}

// UpdateMetadata 实现 sink.MetadataSink：DMAP（同 V1）+ SupportsBPlistNowPlaying
// 时后补 POST /command updateMRPlaybackState。
func (s *Sink) UpdateMetadata(m sink.NowPlaying) error {
	cl, uri, cur, _ := s.metadataTarget()
	if cl == nil {
		return nil
	}
	var e airplay.DMAPEncoder
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := cl.Do(ctx, "SET_PARAMETER", uri, map[string]string{
		"Content-Type": "application/x-dmap-tagged",
		"RTP-Info":     "rtptime=" + strconv.FormatUint(uint64(cur), 10),
	}, body); err != nil {
		return err
	}
	return s.updatePlaybackState(ctx, cl)
}

// updatePlaybackState POST /command {type:updateMRPlaybackState,
// params:{mrPlaybackState:1}}（对齐 UpdatePlaybackState；Roon 只发 1）。
func (s *Sink) updatePlaybackState(ctx context.Context, cl *RTSPClient) error {
	if !s.opts.SupportsBPlistNowPlaying {
		return nil
	}
	body, err := airplay.EncodeBplist(map[string]any{
		"type":   "updateMRPlaybackState",
		"params": map[string]any{"mrPlaybackState": 1},
	})
	if err != nil {
		return err
	}
	if _, err := cl.Do(ctx, "POST", "/command", map[string]string{
		"Content-Type": "application/x-apple-binary-plist",
	}, body); err != nil {
		// 对齐 Roon：不被接受只记日志。
		slog.Debug("raop2: updateMRPlaybackState 未被接受", "device", s.id, "err", err)
	}
	return nil
}

// UpdateProgress 实现 sink.ProgressSink（progress: start/cur/end，同 V1）。
func (s *Sink) UpdateProgress(pos, total time.Duration) error {
	cl, uri, cur, rate := s.metadataTarget()
	if cl == nil {
		return nil
	}
	posSamples := int64(pos.Seconds() * float64(rate))
	totalSamples := int64(total.Seconds() * float64(rate))
	start := cur - uint32(posSamples)
	end := start + uint32(totalSamples)
	body := []byte("progress: " + strconv.FormatUint(uint64(start), 10) + "/" +
		strconv.FormatUint(uint64(cur), 10) + "/" + strconv.FormatUint(uint64(end), 10) + "\r\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cl.Do(ctx, "SET_PARAMETER", uri, map[string]string{
		"Content-Type": "text/parameters",
		"RTP-Info":     "rtptime=" + strconv.FormatUint(uint64(cur), 10),
	}, body)
	return err
}

// UpdateArtwork 实现 sink.ArtworkSink（image/jpeg，同 V1）。
func (s *Sink) UpdateArtwork(path string) error {
	cl, uri, cur, _ := s.metadataTarget()
	if cl == nil {
		return nil
	}
	ctype := "image/none"
	var raw []byte
	if path != "" {
		data, err := readFileAll(path)
		if err != nil {
			return err
		}
		raw = data
		ctype = "image/jpeg"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := cl.Do(ctx, "SET_PARAMETER", uri, map[string]string{
		"Content-Type": ctype,
		"RTP-Info":     "rtptime=" + strconv.FormatUint(uint64(cur), 10),
	}, raw)
	return err
}

func (s *Sink) metadataTarget() (*RTSPClient, string, uint32, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rtsp == nil {
		return nil, "", 0, 0
	}
	return s.rtsp, s.uri(), s.rtpBase + uint32(s.audibleSampleLocked()), s.opts.SampleRate
}
