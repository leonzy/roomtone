// Package sink 定义音频输出端的协议层抽象。
//
// 三类 sink：
//   - PushSink（raop、local）：服务端主动推帧，帧上带统一的 play_at；
//   - PullSink（dlna）：服务端只提供流地址，设备自己拉；
//   - 纯控制 sink（local 的纯控制器形态）：只下指令，不接收帧。
//
// 之所以把 Caps/Format/Frame 放在 sink 而不是 player：这些是协议事实，
// player 只做编排。player 侧通过类型别名复用，避免两个包互相 import。
package sink

import (
	"context"
	"time"
)

// Kind 设备类型，与 players.kind 一致。
type Kind string

const (
	KindLocal Kind = "local"
	KindRAOP  Kind = "raop"
	KindDLNA  Kind = "dlna"
)

// Codec 编码格式。l16 是裸 PCM（RAOP 首选），wav 给 DLNA。
type Codec string

const (
	CodecL16  Codec = "l16"
	CodecWAV  Codec = "wav"
	CodecFLAC Codec = "flac"
	CodecAAC  Codec = "aac"
	CodecMP3  Codec = "mp3"
)

// SyncTier 同步等级。组内取最差的一个。
type SyncTier int

const (
	// SyncExact RAOP / local：±20ms，靠 RTP/play_at 对齐。
	SyncExact SyncTier = iota
	// SyncAligned DLNA：只能对齐起播，之后会漂移。
	SyncAligned
)

func (t SyncTier) String() string {
	if t == SyncExact {
		return "exact"
	}
	return "aligned"
}

// Caps 设备能力。JSON 存进 players.capabilities。
type Caps struct {
	Codecs         []Codec `json:"codecs"`
	MaxSampleRate  int     `json:"max_sample_rate"`
	Channels       int     `json:"channels"`
	BitDepth       int     `json:"bit_depth"`
	SupportsVolume bool    `json:"supports_volume"`
	SupportsSeek   bool    `json:"supports_seek"`
	// LatencyMs 设备固有缓冲延迟（RAOP 默认 2000ms），用于时间轴对齐。
	LatencyMs int      `json:"latency_ms"`
	SyncTier  SyncTier `json:"sync_tier"`
	// NeedsPassword 设备要求密码（mDNS TXT pw=true），第一版直接跳过。
	NeedsPassword bool `json:"needs_password"`
	// DeviceID RAOP 的 RTSP 路径 id（mDNS 服务名里的 MAC）。
	// 来自设备广播，因此随 capabilities 一起存，不单独建列。
	DeviceID string `json:"device_id,omitempty"`
	// PublicKey RAOP 加密用的 PEM 公钥（TXT pk）。为空表示只能明文。
	PublicKey string `json:"public_key,omitempty"`
}

// Format 组内统一后的输出格式（协商结果）。
type Format struct {
	Codec      Codec `json:"codec"`
	SampleRate int   `json:"sample_rate"`
	Channels   int   `json:"channels"`
	Bits       int   `json:"bits"`
}

// BytesPerSample 一帧 PCM 单声道的字节数。
func (f Format) BytesPerSample() int { return f.Bits / 8 }

// FrameBytes 一个采样帧（所有声道）的字节数。
func (f Format) FrameBytes() int { return f.BytesPerSample() * f.Channels }

// EQ 执行方。防"双重 EQ"的唯一开关：服务端与客户端只允许一方应用曲线，
// 两边都做会把 -6dB 叠成 -12dB，用户听到的是明显过头的音色。
const (
	// EQAppliedByServer 服务端在分发之后逐样本处理（RAOP / DLNA 这类服务端推帧的场景）。
	EQAppliedByServer = "server"
	// EQAppliedByClient 客户端自拉流，服务端不下发 PCM，由客户端按曲线自行处理。
	EQAppliedByClient = "client"
)

// EQSettings 下发给客户端（或留在服务端）的 EQ 状态。
//
// Bands 顺序固定对应 31/62/125/250/500/1k/2k/4k/8k/16k Hz，Preamp 已由服务端
// 按最大正增益算好（≤0），客户端直接用，不要自己再算一遍。
type EQSettings struct {
	Enabled   bool      `json:"enabled"`
	Bands     []float64 `json:"bands"`
	Preamp    float64   `json:"preamp"`
	AppliedBy string    `json:"applied_by"`
}

// StreamPlan 下发给 sink 的播放计划。
type StreamPlan struct {
	Format   Format
	SongID   int64
	Title    string
	Position time.Duration
	Duration time.Duration
	// Latency 起播提前量：帧在 play_at 之前这么久就已经写好。
	Latency time.Duration
	// StreamURL 仅 PullSink 使用（局域网可达绝对地址）。
	StreamURL string
}

// Frame 一帧 PCM。SampleIndex 是组内统一时间轴上的绝对采样序号（不随切歌归零），
// PlayAt 是该帧第一个采样应在墙上时间何时发声。
type Frame struct {
	SampleIndex int64
	PlayAt      time.Time
	Data        []byte
}

// EventType sink 主动上报的事件类型。
type EventType string

const (
	EventBuffering EventType = "buffering"
	EventPlaying   EventType = "playing"
	EventPaused    EventType = "paused"
	EventStopped   EventType = "stopped"
	EventProgress  EventType = "progress"
	EventOffline   EventType = "offline"
	EventError     EventType = "error"
)

// Event sink → 引擎的上报。
type Event struct {
	Type     EventType
	Position time.Duration
	Err      error
	Detail   string
}

// Sink 所有输出端的公共部分。
type Sink interface {
	ID() string
	Kind() Kind
	Caps() Caps
	Open(ctx context.Context, plan StreamPlan) error
	Start(at time.Time) error
	Seek(sample uint64) error
	SetVolume(v float64) error
	Stop() error
	Close() error
	Events() <-chan Event
}

// PushSink 接收 PCM 帧。
type PushSink interface {
	Sink
	WriteFrame(f Frame) error
}

// PowerSink 支持待机 / 唤醒的输出端。
//
// 只有真正能"安静下来又不丢会话"的设备才实现：DLNA 发 Stop 并清空 URI；
// RAOP 的 RTSP 没有标准待机指令，实现只是停发 RTP（TEARDOWN），
// 因此**不得**对外宣称设备已进入硬件待机——UI 上只标 standby。
type PowerSink interface {
	Standby(ctx context.Context) error
	Wake(ctx context.Context) error
}

// PullSink 只提供流地址。
type PullSink interface {
	Sink
	StreamURL(plan StreamPlan) (string, error)
}
