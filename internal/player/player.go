package player

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"songloft/internal/player/sink"
)

// Kind / Caps / Format / SyncTier 在 sink 包定义，player 侧用别名，
// 调用方（handler / service）只 import player 就够了。
type (
	Kind     = sink.Kind
	Caps     = sink.Caps
	Format   = sink.Format
	SyncTier = sink.SyncTier
	Codec    = sink.Codec
)

const (
	KindLocal = sink.KindLocal
	KindRAOP  = sink.KindRAOP
	KindDLNA  = sink.KindDLNA

	SyncExact   = sink.SyncExact
	SyncAligned = sink.SyncAligned

	CodecL16  = sink.CodecL16
	CodecWAV  = sink.CodecWAV
	CodecFLAC = sink.CodecFLAC
	CodecAAC  = sink.CodecAAC
	CodecMP3  = sink.CodecMP3
)

// 音量模式。存 players.volume_mode。
const (
	// VolumeModeDevice 音量直接下发到设备（默认）。
	VolumeModeDevice = "device"
	// VolumeModeFixed 固定音量：忽略 SetVolume，保持当前值。
	VolumeModeFixed = "fixed"
	// VolumeModeDSP 音量在服务端 DSP 管线里应用。
	VolumeModeDSP = "dsp"

	// DefaultMaxVolume 新设备的默认音量上限。
	DefaultMaxVolume = 100
)

// TakeoverBehavior 本机被别的目标切走时的行为。
type TakeoverBehavior string

const (
	// TakeoverPause 立即暂停并提示（默认）。
	TakeoverPause TakeoverBehavior = "pause"
	// TakeoverFollow 静默转成纯控制器。
	TakeoverFollow TakeoverBehavior = "follow"
)

// EQBandCount 10 段 EQ，与客户端 /settings/equalizer 一致。
const EQBandCount = 10

// EQProfile 每音箱均衡器配置。存 players.eq_profile。
type EQProfile struct {
	Enabled bool      `json:"enabled"`
	Preset  string    `json:"preset"`
	Bands   []float64 `json:"bands"`  // 10 段增益 dB
	Preamp  float64   `json:"preamp"` // 服务端算出的前级衰减，防削波
}

// Flat 是否等价于不处理（决定要不要强制走解码管线）。
func (p EQProfile) Flat() bool {
	if !p.Enabled {
		return true
	}
	for _, b := range p.Bands {
		if b != 0 {
			return false
		}
	}
	return true
}

// Player 一台播放设备（内存态）。
type Player struct {
	ID         string
	Kind       Kind
	Name       string
	Address    string
	Model      string
	Caps       Caps
	Volume     int
	Muted      bool
	VolumeMode string `json:"volume_mode"`
	MaxVolume  int    `json:"max_volume"`
	Online     bool
	Takeover   TakeoverBehavior
	EQ         EQProfile
	// Standby 已待机（不再收帧、不进同步环）。只存内存，重启后按物理状态重新探测。
	Standby bool `json:"standby,omitempty"`
	// Password 受密码保护设备的密码（AirPlay 1 = RTSP HTTP Digest，见
	// raop/digest.go）。json:"-" 防泄漏：不出 API，更新走 PlayerUpdate.Password
	// 单独通道。
	Password string `json:"-"`
}

// SupportsCodec 是否支持某编码。
func (p *Player) SupportsCodec(c Codec) bool {
	for _, x := range p.Caps.Codecs {
		if x == c {
			return true
		}
	}
	return false
}

// Member 组成员。
type Member struct {
	PlayerID     string `json:"player_id"`
	DelayMs      int    `json:"delay_ms"`
	VolumeOffset int    `json:"volume_offset"`
	// Position 组内顺序，决定列表顺序与"主控候选"顺序（0 起）。
	Position int `json:"position"`
}

// Group 同步组。单机也有一个只含自己的组——队列与传输逻辑只写一份。
type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Icon 区域图标 key（白名单见 manager.go），空串表示客户端默认图标。
	Icon     string   `json:"icon"`
	MasterID string   `json:"master_id"`
	PartyID  string   `json:"party_id,omitempty"`
	Members  []Member `json:"members"`
}

// MemberIDs 成员 id 列表。
func (g *Group) MemberIDs() []string {
	out := make([]string, 0, len(g.Members))
	for _, m := range g.Members {
		out = append(out, m.PlayerID)
	}
	return out
}

// HasMember 是否包含某设备。
func (g *Group) HasMember(id string) bool {
	for _, m := range g.Members {
		if m.PlayerID == id {
			return true
		}
	}
	return false
}

// codecRank 编码优先级：无损优先，但以"能出声"兜底（§4.5）。
var codecRank = map[Codec]int{
	CodecL16:  0,
	CodecFLAC: 1,
	CodecWAV:  2,
	CodecAAC:  3,
	CodecMP3:  4,
}

// NegotiateFormat 组内格式协商。
//
// 规则：
//  1. 采样率取所有成员上限的**最小值**（混入只支持 48k 的设备则整组 48k）；
//  2. 编码按兼容性降级 l16 → flac → wav → aac → mp3，取所有成员都支持的最好一档；
//  3. 声道统一 2ch，位深统一 16bit（RAOP 与 DLNA 都吃这个组合）。
func NegotiateFormat(players []*Player) (Format, error) {
	if len(players) == 0 {
		return Format{}, fmt.Errorf("negotiate format: no players")
	}
	rate := 0
	bestRank := len(codecRank) + 1
	var bestCodec Codec
	for _, p := range players {
		r := p.Caps.MaxSampleRate
		if r <= 0 {
			r = 44100
		}
		if rate == 0 || r < rate {
			rate = r
		}
		for _, c := range p.Caps.Codecs {
			rank, ok := codecRank[c]
			if !ok {
				continue
			}
			if rank < bestRank {
				// 只有所有成员都支持时才算数，先记录候选，下面统一校验。
				bestRank = rank
				bestCodec = c
			}
		}
	}
	// 校验候选是否全员支持；不支持就按优先级往下退。
	candidates := make([]Codec, 0, len(codecRank))
	for c := range codecRank {
		candidates = append(candidates, c)
	}
	sort.Slice(candidates, func(i, j int) bool { return codecRank[candidates[i]] < codecRank[candidates[j]] })
	for _, c := range candidates {
		all := true
		for _, p := range players {
			if !p.SupportsCodec(c) {
				all = false
				break
			}
		}
		if all {
			bestCodec = c
			break
		}
	}
	if bestCodec == "" {
		return Format{}, fmt.Errorf("negotiate format: no common codec")
	}
	return Format{Codec: bestCodec, SampleRate: rate, Channels: 2, Bits: 16}, nil
}

// GroupSyncTier 组同步等级：取最差成员。含任一 DLNA 即整组降级。
func GroupSyncTier(players []*Player) SyncTier {
	tier := SyncExact
	for _, p := range players {
		if p.Caps.SyncTier > tier {
			tier = p.Caps.SyncTier
		}
	}
	return tier
}

// CapsJSON 序列化能力（落库）。
func CapsJSON(c Caps) string {
	if len(c.Codecs) == 0 {
		c.Codecs = []Codec{CodecL16}
	}
	data, err := json.Marshal(c)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// ParseCaps 反序列化能力，坏数据退回默认（宁可播出声，不要因为一行 JSON 起不来）。
func ParseCaps(raw string) Caps {
	c := Caps{}
	if strings.TrimSpace(raw) == "" {
		return defaultCaps()
	}
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return defaultCaps()
	}
	if len(c.Codecs) == 0 {
		c.Codecs = defaultCaps().Codecs
	}
	if c.MaxSampleRate <= 0 {
		c.MaxSampleRate = 44100
	}
	if c.Channels <= 0 {
		c.Channels = 2
	}
	if c.BitDepth <= 0 {
		c.BitDepth = 16
	}
	return c
}

func defaultCaps() Caps {
	return Caps{
		Codecs:         []Codec{CodecL16, CodecWAV, CodecMP3},
		MaxSampleRate:  44100,
		Channels:       2,
		BitDepth:       16,
		SupportsVolume: true,
		SupportsSeek:   true,
		LatencyMs:      2000,
		SyncTier:       SyncExact,
	}
}

// EQJSON 序列化 EQ。
func EQJSON(e EQProfile) string {
	data, err := json.Marshal(e)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// ParseEQ 反序列化 EQ。
func ParseEQ(raw string) EQProfile {
	e := EQProfile{Bands: make([]float64, EQBandCount)}
	if strings.TrimSpace(raw) == "" {
		return e
	}
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		return EQProfile{Bands: make([]float64, EQBandCount)}
	}
	if len(e.Bands) != EQBandCount {
		bands := make([]float64, EQBandCount)
		copy(bands, e.Bands)
		e.Bands = bands
	}
	return e
}
