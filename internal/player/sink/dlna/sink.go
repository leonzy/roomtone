package dlna

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"songloft/internal/player/sink"
)

// Options 建 sink 的参数。
type Options struct {
	// CallbackBase 事件回调基地址（局域网可达），空则不做订阅。
	CallbackBase string
	// PollInterval 轮询间隔，默认 3s。
	PollInterval time.Duration
	// CommandTimeout 单条 SOAP 超时。
	CommandTimeout time.Duration
}

// Sink DLNA 渲染器输出端（PullSink：设备自己来拉流）。
type Sink struct {
	id     string
	dev    *Device
	cl     *Client
	patch  Patch
	caps   sink.Caps
	opts   Options
	events chan sink.Event
	// eventsMu 保护 channel 的发送与关闭：Close 拿写锁后再 close，
	// 避免"事件 goroutine 还在发、channel 已经被关"的 panic。
	eventsMu sync.RWMutex

	mu       sync.Mutex
	plan     sink.StreamPlan
	url      string
	started  bool
	playing  bool
	closed   bool
	volume   int
	position time.Duration
	startAt  time.Time
	sub      *Subscription
	done     chan struct{}
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	failures int
}

// New 创建 sink。dev 必须含 AVTransport 服务。
func New(id string, dev *Device, cl *Client, opts Options) *Sink {
	if cl == nil {
		cl = NewClient()
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 3 * time.Second
	}
	if opts.CommandTimeout <= 0 {
		opts.CommandTimeout = 10 * time.Second
	}
	patch := PatchFor(dev.ModelName)
	return &Sink{
		id:     id,
		dev:    dev,
		cl:     cl,
		patch:  patch,
		opts:   opts,
		events: make(chan sink.Event, 32),
		done:   make(chan struct{}),
		volume: 100,
		caps:   CapsFor(dev),
	}
}

func (s *Sink) ID() string                { return s.id }
func (s *Sink) Kind() sink.Kind           { return sink.KindDLNA }
func (s *Sink) Caps() sink.Caps           { return s.caps }
func (s *Sink) Events() <-chan sink.Event { return s.events }
func (s *Sink) Patch() Patch              { return s.patch }

// CapsFor 设备能力。发现与建 sink 共用一份，避免两边写法漂移。
//
// DLNA 没有能力协商接口（GetProtocolInfo 只在部分设备上可用且格式各异），
// 这里给保守值：WAV/FLAC/MP3 都能推，采样率上限 48k，同步等级只能对齐起播。
func CapsFor(dev *Device) sink.Caps {
	return sink.Caps{
		Codecs:         []sink.Codec{sink.CodecWAV, sink.CodecFLAC, sink.CodecMP3},
		MaxSampleRate:  48000,
		Channels:       2,
		BitDepth:       16,
		SupportsVolume: dev != nil && dev.Services[SvcRenderingControl].ControlURL != nil,
		SupportsSeek:   true,
		LatencyMs:      500,
		SyncTier:       sink.SyncAligned,
	}
}

// StreamURL 实现 sink.PullSink：DLNA 只拿到地址，不接收帧。
func (s *Sink) StreamURL(plan sink.StreamPlan) (string, error) {
	if plan.StreamURL == "" {
		return "", errors.New("dlna: 缺少流地址")
	}
	return plan.StreamURL, nil
}

// Open 装载媒体：SetAVTransportURI → (Seek) → 起轮询。
//
// 真正 Play 在 Start，这样组内多台设备能对齐起播时刻。
func (s *Sink) Open(ctx context.Context, plan sink.StreamPlan) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("dlna: sink 已关闭")
	}
	if s.started {
		s.mu.Unlock()
		return errors.New("dlna: 已经 Open")
	}
	s.plan = plan
	s.url = plan.StreamURL
	if s.url == "" {
		s.mu.Unlock()
		return errors.New("dlna: 缺少流地址")
	}
	s.started = true
	s.position = plan.Position
	meta := BuildMetadata(Item{
		Title:    plan.Title,
		URL:      plan.StreamURL,
		MimeType: mimeOf(plan.Format.Codec),
		Duration: formatDuration(plan.Duration),
	})
	seekTo := plan.Position
	s.mu.Unlock()

	if err := s.call(ctx, SvcAVTransport, "SetAVTransportURI",
		Arg{"InstanceID", "0"},
		Arg{"CurrentURI", plan.StreamURL},
		Arg{"CurrentURIMetaData", meta},
	); err != nil {
		return err
	}
	if seekTo > 0 {
		if err := s.seekTo(ctx, seekTo); err != nil {
			return err
		}
	}

	s.emit(sink.Event{Type: sink.EventBuffering})

	// 事件订阅尽力而为：失败就靠轮询。
	if s.opts.CallbackBase != "" && !s.patch.DisableEvents {
		s.trySubscribe()
	}
	s.startPoller()
	return nil
}

// Start 在 at 时刻下发 Play。
func (s *Sink) Start(at time.Time) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("dlna: sink 已关闭")
	}
	s.startAt = at
	// 在锁内 Add，保证 Close 的 Wait 不会与 Add 并发。
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		if d := time.Until(at); d > 0 {
			select {
			case <-time.After(d):
			case <-s.done:
				return
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), s.opts.CommandTimeout)
		defer cancel()
		if err := s.call(ctx, SvcAVTransport, "Play", Arg{"InstanceID", "0"}, Arg{"Speed", "1"}); err != nil {
			s.emit(sink.Event{Type: sink.EventError, Err: err})
			return
		}
		s.mu.Lock()
		s.playing = true
		s.mu.Unlock()
		s.emit(sink.Event{Type: sink.EventPlaying})
	}()
	return nil
}

// Seek 跳到指定采样。
func (s *Sink) Seek(sample uint64) error {
	s.mu.Lock()
	rate := s.plan.Format.SampleRate
	if rate <= 0 {
		rate = 44100
	}
	pos := time.Duration(float64(sample) / float64(rate) * float64(time.Second))
	s.position = pos
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.CommandTimeout)
	defer cancel()
	return s.seekTo(ctx, pos)
}

// SetVolume 音量 0~1 → 设备 0~100。
func (s *Sink) SetVolume(v float64) error {
	if v < 0 {
		v = 0
	}
	if v > 1 {
		v = 1
	}
	vol := int(v*100 + 0.5)
	s.mu.Lock()
	s.volume = vol
	s.mu.Unlock()
	if s.dev.Services[SvcRenderingControl].ControlURL == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.CommandTimeout)
	defer cancel()
	return s.call(ctx, SvcRenderingControl, "SetVolume",
		Arg{"InstanceID", "0"},
		Arg{"Channel", "Master"},
		Arg{"DesiredVolume", strconv.Itoa(vol)},
	)
}

// Stop 停止播放。
func (s *Sink) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.CommandTimeout)
	defer cancel()
	err := s.call(ctx, SvcAVTransport, "Stop", Arg{"InstanceID", "0"})
	s.mu.Lock()
	s.playing = false
	s.started = false
	s.mu.Unlock()
	s.emit(sink.Event{Type: sink.EventStopped})
	return err
}

// Standby 待机：Stop + 清空 CurrentURI。
//
// 只发 Stop 不够——很多 renderer 会把最后一条 URI 留在界面上继续显示
// "正在播放 xxx"，用户以为还在放。清空 URI 才是真的停干净。
func (s *Sink) Standby(ctx context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.playing = false
	s.started = false
	s.mu.Unlock()
	if err := s.call(ctx, SvcAVTransport, "Stop", Arg{"InstanceID", "0"}); err != nil {
		return err
	}
	if err := s.call(ctx, SvcAVTransport, "SetAVTransportURI",
		Arg{"InstanceID", "0"}, Arg{"CurrentURI", ""}, Arg{"CurrentURIMetaData", ""}); err != nil {
		return err
	}
	s.emit(sink.Event{Type: sink.EventStopped})
	return nil
}

// Wake 唤醒：DLNA 没有"上电"指令，下一次 Open 时 SetAVTransportURI 即唤醒。
func (s *Sink) Wake(context.Context) error { return nil }

// Close 退订并停掉轮询。
func (s *Sink) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.done)
	cancel := s.cancel
	sub := s.sub
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
	if sub != nil {
		ctx, c := context.WithTimeout(context.Background(), 3*time.Second)
		sub.Unsubscribe(ctx)
		c()
	}
	s.eventsMu.Lock()
	close(s.events)
	s.eventsMu.Unlock()
	return nil
}

// HandleNotify 处理设备推来的 GENA 事件（宿主收到 NOTIFY 后调用）。
func (s *Sink) HandleNotify(body []byte) {
	vars := ParseLastChange(body)
	if vars == nil {
		return
	}
	if v, ok := vars["TransportState"]; ok {
		s.onTransportState(v)
	}
	if v, ok := vars["RelativeTimePosition"]; ok {
		s.mu.Lock()
		s.position = parseDuration(v)
		pos := s.position
		s.mu.Unlock()
		s.emit(sink.Event{Type: sink.EventProgress, Position: pos})
	}
	if v, ok := vars["Volume"]; ok {
		if n, err := strconv.Atoi(v); err == nil {
			s.mu.Lock()
			s.volume = n
			s.mu.Unlock()
		}
	}
}

// PlaybackPosition 实现 sink.PositionReporter：轮询得到的进度，只在对端在播时有效。
func (s *Sink) PlaybackPosition() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.started || s.closed {
		return 0, false
	}
	if !s.playing {
		return 0, false
	}
	return s.position, true
}

// Position 当前播放位置（轮询/事件更新的最近值）。
func (s *Sink) Position() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.position
}

// --- 内部 ---

func (s *Sink) call(ctx context.Context, svcName, action string, args ...Arg) error {
	svc, ok := s.dev.Services[svcName]
	if !ok {
		return fmt.Errorf("dlna: 设备不支持 %s", svcName)
	}
	_, err := s.cl.Call(ctx, svc, action, args...)
	if err != nil {
		s.mu.Lock()
		s.failures++
		fails := s.failures
		s.mu.Unlock()
		if fails >= 3 {
			s.emit(sink.Event{Type: sink.EventOffline, Err: err})
		}
		return err
	}
	s.mu.Lock()
	s.failures = 0
	s.mu.Unlock()
	return nil
}

func (s *Sink) seekTo(ctx context.Context, pos time.Duration) error {
	unit := s.patch.SeekUnit
	if unit == "" {
		unit = "REL_TIME"
	}
	return s.call(ctx, SvcAVTransport, "Seek",
		Arg{"InstanceID", "0"},
		Arg{"Unit", unit},
		Arg{"Target", formatDuration(pos)},
	)
}

func (s *Sink) trySubscribe() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	svc, ok := s.dev.Services[SvcAVTransport]
	if !ok {
		return
	}
	cb := s.opts.CallbackBase + "/api/v1/playback/dlna/notify/" + s.id
	sub, err := Subscribe(ctx, s.cl, svc, cb, 300*time.Second)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.sub = sub
	s.mu.Unlock()
}

func (s *Sink) startPoller() {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancel = cancel
	s.mu.Unlock()

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		interval := s.opts.PollInterval
		s.mu.Lock()
		if s.sub != nil {
			// 有事件时轮询只当兜底，拉长到 10s。
			interval = 10 * time.Second
		}
		s.mu.Unlock()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.poll(ctx)
			}
		}
	}()
}

func (s *Sink) poll(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, s.opts.CommandTimeout)
	defer cancel()
	svc, ok := s.dev.Services[SvcAVTransport]
	if !ok {
		return
	}
	out, err := s.cl.Call(ctx, svc, "GetTransportInfo", Arg{"InstanceID", "0"})
	if err != nil {
		s.mu.Lock()
		s.failures++
		fails := s.failures
		s.mu.Unlock()
		if fails >= 3 {
			s.emit(sink.Event{Type: sink.EventOffline, Err: err})
		}
		return
	}
	s.mu.Lock()
	s.failures = 0
	s.mu.Unlock()
	if st := out["CurrentTransportState"]; st != "" {
		s.onTransportState(st)
	}
	out, err = s.cl.Call(ctx, svc, "GetPositionInfo", Arg{"InstanceID", "0"})
	if err != nil {
		return
	}
	pos := parseDuration(out["RelTime"])
	s.mu.Lock()
	s.position = pos
	s.mu.Unlock()
	s.emit(sink.Event{Type: sink.EventProgress, Position: pos})
}

func (s *Sink) onTransportState(st string) {
	s.mu.Lock()
	prevPlaying := s.playing
	s.playing = st == "PLAYING"
	nowPlaying := s.playing
	s.mu.Unlock()
	switch st {
	case "PLAYING":
		if !prevPlaying {
			s.emit(sink.Event{Type: sink.EventPlaying})
		}
	case "PAUSED_PLAYBACK":
		s.emit(sink.Event{Type: sink.EventPaused})
	case "STOPPED", "NO_MEDIA_PRESENT":
		if prevPlaying || nowPlaying {
			s.emit(sink.Event{Type: sink.EventStopped})
		}
	case "TRANSITIONING":
		s.emit(sink.Event{Type: sink.EventBuffering})
	}
}

func (s *Sink) emit(ev sink.Event) {
	s.eventsMu.RLock()
	defer s.eventsMu.RUnlock()
	select {
	case s.events <- ev:
	default:
	}
}

func mimeOf(c sink.Codec) string {
	switch c {
	case sink.CodecFLAC:
		return "audio/flac"
	case sink.CodecMP3:
		return "audio/mpeg"
	case sink.CodecAAC:
		return "audio/aac"
	case sink.CodecL16:
		return "audio/L16;rate=44100;channels=2"
	default:
		return "audio/wav"
	}
}
