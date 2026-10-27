package raat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"songloft/internal/player/sink"
)

// Options 建 sink 的参数。
type Options struct {
	// Address 设备控制通道地址 host:port。
	Address string
	// SampleRate / Channels 协商后的输出格式。
	SampleRate int
	Channels   int
	// BufferSize / ReadyThreshold stream 请求的缓冲参数。
	// 约束：BufferSize=2s 与 pipeline.DefaultBufferDepth 对齐、
	// ReadyThreshold=1s 与 transport.DefaultBufferWatermark 对齐——
	// 不触碰 guards.sh 守护的两个全局常量。
	BufferSize     time.Duration
	ReadyThreshold time.Duration
	// OnControl 设备反控回调（subscribe_transport）。nil 不启用。
	OnControl func(cmd string, value float64) error
	// FLAC 压缩音频通道（B9）：info 声明 codecs 含 "flac" 时开启，
	// setup 带 wire_codec:"flac"，WriteFrame 走 CODEC_DATA=5。
	FLAC bool
	// DialTimeout 建连超时。
	DialTimeout time.Duration
}

// Sink RAAT 输出端（RaatZonePlayer 的 Core 侧对应物）。
//
// Sink 接口与 RAAT 动作一一映射（计划 B5 表）：
//
//	Open    Dial 控制 → info → set_client_type("roomtone") → setup →
//	        Dial 音频 → STREAMID → stream（流式响应→Events）→ keepalive
//	Start   start{min_offset:200ms}（无 time，P0 模式；对齐 Roon 单端点
//	        行为 RaatZonePlayer.cs:1903-1913：设备在响应里回报实际起播
//	        时刻 time，到达时刻锚定 wallStart）
//	WriteFrame  audio.WritePCM(f.SampleIndex - anchorSample, f.Data)
//	        （stream_sample 为流内相对位置）
//	Seek    stop → stream_id++ → STREAMID → stream（等 Ready）→ start
//	        （RAAT 无原生 seek，重建流；anchorSample 不变——绝对时间轴）
//	SetVolume   update_volume{value: v×100}
//	Continue    仅更新 plan（对齐 raop.Continue）
type Sink struct {
	id     string
	opts   Options
	caps   sink.Caps
	events chan sink.Event

	mu sync.Mutex
	// screen 设备屏显状态（update_transport 全量推送的缓存）。
	screen screenState
	// ctrlMu/lastControl* 反控记录（subscribe_transport 通知 → 测试断言）。
	ctrlMu         sync.Mutex
	lastControlCmd string
	lastControlVal float64
	// cbMu 保护流式回调与 API 方法共享的状态（ready/started/dropouts/
	// wallStart）。不能复用 s.mu：Open 全程持 s.mu 等 stream 的流式响应，
	// 回调若再取 s.mu 会死锁（readLoop 被卡住无法送出终段响应）。
	// 锁序恒为 mu → cbMu。
	cbMu         sync.Mutex
	ctrl         *Conn
	audio        *AudioConn
	streamID     int32
	anchorSample int64 // = plan.Position 采样（绝对时间轴锚点）
	startAt      time.Time
	wallStart    time.Time
	started      bool
	ready        bool
	plan         sink.StreamPlan
	volume       float64
	clockPort    int
	dropouts     uint64
	closed       bool
	// transportSupported info.transport.is_supported（屏显/反控开关）。
	transportSupported bool
	// clock 时钟源（SyncToClock 后非 nil）；precise 时钟同步成功标记。
	clock   *ClockSource
	precise bool
	// flacSetup setup 已声明 wire_codec:"flac"（WriteFrame 走 CODEC_DATA）。
	flacSetup bool
	// deviceCodecs info 声明的线编码列表（如 ["flac"]）。
	deviceCodecs []string
	// deviceCodeSigningKeys info 声明的代码签名公钥名列表（load_script 用）。
	deviceCodeSigningKeys []string
	// signProxy raat-sign 签名代理（LoadScript 用；nil 时默认构造）。
	signProxy *SignProxy
}

// New 创建 sink。真正建连在 Open。
func New(id string, opts Options) *Sink {
	if opts.SampleRate == 0 {
		opts.SampleRate = 44100
	}
	if opts.Channels == 0 {
		opts.Channels = 2
	}
	if opts.BufferSize == 0 {
		opts.BufferSize = 2 * time.Second
	}
	if opts.ReadyThreshold == 0 {
		opts.ReadyThreshold = time.Second
	}
	caps := sink.Caps{
		Codecs:         []sink.Codec{sink.CodecL16},
		MaxSampleRate:  opts.SampleRate,
		Channels:       opts.Channels,
		BitDepth:       16,
		SupportsVolume: true,
		SupportsSeek:   true,
		LatencyMs:      int(opts.BufferSize / time.Millisecond),
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
func (s *Sink) Kind() sink.Kind           { return sink.KindRAAT }
func (s *Sink) Caps() sink.Caps           { return s.caps }
func (s *Sink) Events() <-chan sink.Event { return s.events }

func (s *Sink) emit(ev sink.Event) {
	select {
	case s.events <- ev:
	default:
	}
}

// infoResponse info 请求的响应形态（只取 Sink 关心的字段）。
type infoResponse struct {
	Status string `json:"status"`
	Output struct {
		SupportedFormats string `json:"supported_formats"`
	} `json:"output"`
	// Codecs 设备声明的线编码列表（RaatTransportClient.cs:751：info 顶层
	// "codecs"，如 ["flac"]）。
	Codecs []string `json:"codecs"`
	Volume struct {
		IsSupported bool `json:"is_supported"`
	} `json:"volume"`
	Transport struct {
		IsSupported bool `json:"is_supported"`
	} `json:"transport"`
	// Info 内层 info（code_signing_keys 在这里，逗号分隔）。
	Info struct {
		CodeSigningKeys string `json:"code_signing_keys"`
	} `json:"info"`
}

// RawCodeSigningKeys 逗号分隔的签名密钥名（空串 = 无）。
func (r *infoResponse) RawCodeSigningKeys() string { return r.Info.CodeSigningKeys }

// hasCodec 设备是否声明支持某线编码。
func (r *infoResponse) hasCodec(name string) bool {
	for _, c := range r.Codecs {
		if c == name {
			return true
		}
	}
	return false
}

// setupResponse setup 请求的响应形态。
type setupResponse struct {
	Status       string `json:"status"`
	ClockPort    int    `json:"clock_port"`
	AudioPortTCP int    `json:"audio_port_tcp"`
}

// Open 建立会话：Dial 控制 → info → set_client_type → setup → Dial 音频 →
// STREAMID → stream → keepalive。
func (s *Sink) Open(ctx context.Context, plan sink.StreamPlan) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("raat: sink 已关闭")
	}
	if s.ctrl != nil {
		return errors.New("raat: 已经 Open")
	}
	if plan.Format.SampleRate > 0 {
		s.opts.SampleRate = plan.Format.SampleRate
	}
	if plan.Format.Channels > 0 {
		s.opts.Channels = plan.Format.Channels
	}
	s.plan = plan
	s.anchorSample = plan.Position.Milliseconds() * int64(s.opts.SampleRate) / 1000
	s.transportSupported = true // info 探测后精确化（B6 的 ProbeInfo 透传）

	addr := s.opts.Address
	if addr == "" {
		return errors.New("raat: 缺少设备地址")
	}
	ctrl, err := DialControl(ctx, addr)
	if err != nil {
		return err
	}
	s.ctrl = ctrl
	// 控制通道断连 → EventError + EventOffline（对齐 dlna 三次失败标离线
	// 的语义；v1 不做播放中重连——与 raop 现状一致，如实标注）。
	go func() {
		<-ctrl.closed
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.ctrl == ctrl && !s.closed {
			s.emit(sink.Event{Type: sink.EventError, Err: ErrClosed})
			s.emit(sink.Event{Type: sink.EventOffline})
		}
	}()

	// info：解析 supported_formats / volume / transport / codecs
	var info infoResponse
	if err := ctrl.RequestSimple(ctx, "info", nil, &info); err != nil {
		s.closeLocked()
		return err
	}
	s.deviceCodecs = info.Codecs

	// set_client_type("roomtone")
	if err := ctrl.RequestSimple(ctx, "set_client_type", map[string]any{"client_type": "roomtone"}, nil); err != nil {
		s.closeLocked()
		return err
	}

	// setup：format{pcm, rate, bits:16, ch}；FLAC 开启且采样率合法时带
	// wire_codec:"flac"（RaatTransportClient.cs:2258；采样率限制
	// ShouldUseFlacWireCodec RaatZonePlayer.cs:2207）。
	setupArgs := map[string]any{
		"format": map[string]any{
			"sample_type":     "pcm",
			"sample_rate":     s.opts.SampleRate,
			"bits_per_sample": 16,
			"channels":        s.opts.Channels,
		},
	}
	// FLAC 三条件：Options 开启 + 设备 info 声明 codecs 含 "flac" +
	// 采样率在 FLAC streamable subset（对齐 RaatZonePlayer.cs:677-680 的
	// flag2 判定：__raat_flac_bit && Client.SupportedCodecs 含 flac &&
	// BitsPerSample != 1 && ShouldUseFlacWireCodec）。
	if s.opts.FLAC && s.deviceHasCodec("flac") && shouldUseFlacWireCodec(s.opts.SampleRate) {
		setupArgs["wire_codec"] = "flac"
	}
	var setup setupResponse
	if err := ctrl.RequestSimple(ctx, "setup", setupArgs, &setup); err != nil {
		s.closeLocked()
		return err
	}
	s.clockPort = setup.ClockPort
	if _, ok := setupArgs["wire_codec"]; ok {
		s.flacSetup = true
	}
	// code_signing_keys：SOOD 通告的键名列表（info 响应 info.code_signing_keys，
	// 逗号分隔）——LoadScript 的签名决策依据。
	if raw := info.RawCodeSigningKeys(); raw != "" {
		s.deviceCodeSigningKeys = splitComma(raw)
	}

	// Dial 音频通道
	audioAddr := hostOfAddr(addr)
	audio, err := DialAudio(ctx, fmt.Sprintf("%s:%d", audioAddr, setup.AudioPortTCP))
	if err != nil {
		s.closeLocked()
		return err
	}
	s.audio = audio

	// STREAMID → stream（流式响应 → Events）
	s.streamID++
	if err := audio.SetStreamID(s.streamID); err != nil {
		s.closeLocked()
		return err
	}
	if err := s.startStreamLocked(ctx); err != nil {
		s.closeLocked()
		return err
	}

	// keepalive(2s)
	ctrl.StartKeepalive(2 * time.Second)

	// 屏显/反控（B7）：transport 支持时订阅设备遥控。
	if s.transportSupported {
		s.subscribeTransport()
	}
	// app channel（B10）：type=2 通知先经 appchannel 分发（含 "channel" 键），
	// 其余走 OnServerRequest（transport 通知）。
	prevHandler := ctrl.OnServerRequest
	ctrl.OnServerRequest = func(raw json.RawMessage) {
		if dispatchAppMessage(s, raw) {
			return
		}
		if prevHandler != nil {
			prevHandler(raw)
		}
	}
	return nil
}

// startStreamLocked 发 stream 请求（流式响应分派事件；Ready 时若已 Start
// 则发 start）。
func (s *Sink) startStreamLocked(ctx context.Context) error {
	args := map[string]any{
		"stream_id":       s.streamID,
		"first_seq":       0,
		"nak_port":        -1, // TCP 音频源，无 NAK 重传（RaatTcpAudioSource 语义）
		"buffer_size":     s.opts.BufferSize.Seconds(),
		"ready_threshold": s.opts.ReadyThreshold.Seconds(),
	}
	ready := make(chan struct{}, 1)
	_, err := s.ctrl.Request(ctx, "stream", args, func(raw json.RawMessage, final bool) error {
		var head struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			return err
		}
		switch head.Status {
		case "Ready":
			s.cbMu.Lock()
			s.ready = true
			started := s.started
			s.cbMu.Unlock()
			s.emit(sink.Event{Type: sink.EventBuffering})
			select {
			case ready <- struct{}{}:
			default:
			}
			if started {
				// Start 先于 Ready 到达：补发 start。异步——回调运行在
				// readLoop 内，阻塞请求会死锁（读循环等自己发出的应答）。
				go func() {
					if err := s.sendStart(context.Background()); err != nil {
						slog.Warn("raat: Ready 后补发 start 失败", "device", s.id, "err", err)
					}
				}()
			}
		case "Playing":
			s.emit(sink.Event{Type: sink.EventPlaying})
		case "Ended":
			s.emit(sink.Event{Type: sink.EventStopped})
		case "Dropout":
			s.cbMu.Lock()
			s.dropouts++
			s.cbMu.Unlock()
			slog.Warn("raat: Dropout", "device", s.id, "total", s.dropouts)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// 等 Ready（500ms 超时；Seek 重建流路径复用）
	select {
	case <-ready:
	case <-time.After(500 * time.Millisecond):
	}
	return nil
}

// sendStart P0 模式：start{min_offset:200ms}（无 time）；响应 time + 到达
// 时刻锚定 wallStart。精度边界：±(RTT/2+处理) ≈ ±10-50ms（SyncExact）；
// ±1ms（SyncPrecise）由 B8 时钟协议补齐。
func (s *Sink) sendStart(ctx context.Context) error {
	var resp struct {
		Status string `json:"status"`
		Time   int64  `json:"time"`
	}
	args := map[string]any{
		"min_offset": 200 * time.Millisecond, // JSON 序列化为 200000000ns
	}
	if s.precise && s.clock != nil && s.clock.OK() {
		// 时钟主时间轴：start 带绝对 time（master 设备时间轴上的起播时刻）
		args["time"] = s.clock.DeviceTime(s.startAt)
	}
	if err := s.ctrl.RequestSimple(ctx, "start", args, &resp); err != nil {
		return err
	}
	// P0：wallStart 已在 Start(at) 锚定为 at（预定发声时刻）。
	// 响应的 time 字段为设备侧起播时刻（B8 时钟协议用它精确化）。
	_ = resp.Time
	return nil
}

func (s *Sink) Start(at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startAt = at
	s.cbMu.Lock()
	s.started = true
	// wallStart 锚定 at（预定发声时刻）：PlaybackPosition =
	// plan.Position + (now - at)。at 通常在未来（engine 传
	// timeline.At(sampleBase)+Delay，预滚期位置为负 → 钳 0）；
	// at 在过去（接管/恢复）则位置立即推进。设备实际起播时刻
	// （start 响应的 time + min_offset）与 at 的偏差即 P0 精度
	// ±(RTT/2+处理) ≈ ±10-50ms；B8 时钟协议补齐到 ±1ms。
	s.wallStart = at
	ready := s.ready
	s.cbMu.Unlock()
	if ready {
		return s.sendStart(context.Background())
	}
	// 未 Ready：由 Ready 事件触发（startStreamLocked 的回调补发）
	return nil
}

func (s *Sink) WriteFrame(f sink.Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.audio == nil {
		return errors.New("raat: 会话未建立")
	}
	// stream_sample 为流内相对位置（f.SampleIndex - anchorSample）
	if s.flacEnabled() {
		// PCM → FLAC 帧（882 采样/帧，与分发帧对齐）→ CODEC_DATA。
		// EncodeFlacFrame 无状态（每帧独立编码），无需持有编码器。
		frameBytes, err := EncodeFlacFrame(f.Data)
		if err != nil {
			return fmt.Errorf("raat: flac 编码失败: %w", err)
		}
		return s.audio.WriteCodec(f.SampleIndex-s.anchorSample, frameBytes)
	}
	return s.audio.WritePCM(f.SampleIndex-s.anchorSample, f.Data)
}

// flacEnabled FLAC 开启且 setup 已声明 wire_codec（设备支持 + 采样率合法）。
func (s *Sink) flacEnabled() bool {
	return s.opts.FLAC && s.flacSetup
}

// deviceHasCodec 设备 info 是否声明支持某线编码。
func (s *Sink) deviceHasCodec(name string) bool {
	for _, c := range s.deviceCodecs {
		if c == name {
			return true
		}
	}
	return false
}

// shouldUseFlacWireCodec 逐字对齐 RaatZonePlayer.cs:2207。
func shouldUseFlacWireCodec(sampleRate int) bool {
	if sampleRate > 0 && sampleRate < 655360 {
		if sampleRate >= 65536 {
			return sampleRate%10 == 0
		}
		return true
	}
	return false
}

func (s *Sink) Seek(sample uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// RAAT 无原生 seek：stop → stream_id++ → STREAMID → stream（等 Ready）→ start
	if err := s.ctrl.RequestSimple(ctx, "stop", nil, nil); err != nil {
		return err
	}
	s.streamID++
	if err := s.audio.SetStreamID(s.streamID); err != nil {
		return err
	}
	s.cbMu.Lock()
	s.ready = false
	started := s.started
	s.cbMu.Unlock()
	if err := s.startStreamLocked(ctx); err != nil {
		return err
	}
	if started {
		return s.sendStart(ctx)
	}
	return nil
}

func (s *Sink) SetVolume(v float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.volume = v
	if s.ctrl == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// number 型设备 0-100；db 型设备量纲待联调定标——诚实边界
	return s.ctrl.RequestSimple(ctx, "update_volume", map[string]any{"value": int(v * 100)}, nil)
}

func (s *Sink) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctrl == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.ctrl.RequestSimple(ctx, "stop", nil, nil)
	s.cbMu.Lock()
	s.started = false
	s.ready = false
	s.cbMu.Unlock()
	s.emit(sink.Event{Type: sink.EventStopped})
	return err
}

func (s *Sink) Continue(plan sink.StreamPlan) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.ctrl == nil {
		return errors.New("raat: 会话已关闭，无法接续")
	}
	s.plan = plan
	s.emit(sink.Event{Type: sink.EventPlaying, Position: plan.Position})
	return nil
}

// ClockEndpoint 实现 sink.ClockParticipant：本设备时钟端点（Open 后有效）。
func (s *Sink) ClockEndpoint() (string, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return hostOfAddr(s.opts.Address), s.clockPort
}

// SyncToClock 实现 sink.ClockParticipant：跟随组内时钟主——
// 设备侧 sync{host,port,new_source:true,quiet:true} + 本地 ClockSource
// 对齐 master。时钟源同步成功后 start 改带 time（master 时间轴换算）、
// Caps 升 SyncPrecise、PlaybackPosition 精确化。
func (s *Sink) SyncToClock(host string, port int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctrl == nil {
		return errors.New("raat: 会话未建立")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// 设备侧 sync：指向时钟主的 clock_port
	if err := s.ctrl.RequestSimple(ctx, "sync", map[string]any{
		"host":       host,
		"port":       port,
		"new_source": true,
		"quiet":      true,
	}, nil); err != nil {
		return err
	}
	// 本地时钟源跟随 master
	if s.clock == nil {
		s.clock = NewClockSource()
	}
	if err := s.clock.Sync(ctx, net.JoinHostPort(host, fmt.Sprint(port))); err != nil {
		// 时钟源同步失败：保留 P0 min_offset 模式（SyncExact），如实标注
		slog.Warn("raat: 时钟源同步失败，保留 P0 模式", "device", s.id, "err", err)
		return nil
	}
	// 成功：升 SyncPrecise；sendStart 改带 time；PlaybackPosition 精确化
	s.caps.SyncTier = sink.SyncPrecise
	s.precise = true
	return nil
}

// Standby 停流（request_standby，transport 支持时）。
func (s *Sink) Standby(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctrl == nil {
		return nil
	}
	return s.ctrl.RequestSimple(ctx, "request_standby", nil, nil)
}

// Wake 唤醒：RAAT 无上电指令，下一次 Open 重新握手。
func (s *Sink) Wake(context.Context) error { return nil }

func (s *Sink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	// end_stream（尽力）
	if s.ctrl != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = s.ctrl.RequestSimple(ctx, "end_stream", nil, nil)
		cancel()
	}
	s.closeLocked()
	close(s.events)
	return nil
}

func (s *Sink) closeLocked() {
	if s.clock != nil {
		s.clock.Close() //nolint:errcheck // 关闭失败无需处理
		s.clock = nil
	}
	if s.audio != nil {
		s.audio.Close() //nolint:errcheck // 关闭失败无需处理
		s.audio = nil
	}
	if s.ctrl != nil {
		s.ctrl.Close() //nolint:errcheck // 同上
		s.ctrl = nil
	}
}

// PlaybackPosition 实现 sink.PositionReporter。
func (s *Sink) PlaybackPosition() (time.Duration, bool) {
	s.cbMu.Lock()
	started, wallStart := s.started, s.wallStart
	s.cbMu.Unlock()
	if !started || wallStart.IsZero() {
		return 0, false
	}
	if s.precise && s.clock != nil && s.clock.OK() {
		// 时钟源精确版：位置 = plan.Position + (master 设备时间 - 起播设备时间)
		elapsed := time.Duration(s.clock.GetTime() - s.clock.DeviceTime(wallStart))
		pos := s.plan.Position + elapsed
		if pos < 0 {
			pos = 0
		}
		return pos, true
	}
	pos := s.plan.Position + time.Since(wallStart)
	if pos < 0 {
		pos = 0
	}
	return pos, true
}

// splitComma 逗号分隔（trim 空白，跳过空项）。
func splitComma(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func hostOfAddr(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i]
		}
	}
	return addr
}
