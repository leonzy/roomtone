package raop2

import (
	"context"
	"encoding/hex"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/mdns"
)

// FeatureFlags 全量位表，逐字来自 AirPlayFeatureFlags.cs。
//
// 注意复合标志（SupportsTransientPairing 等）是多位组合；C# 的
// `(Features & flag) != None` 语义是**任一位命中**即为真。
const (
	FeatureSupportsAirPlayAudio                  uint64 = 0x200
	FeatureSupportsAudioRedundancy               uint64 = 0x800
	FeatureSupportsFairPlayAuth                  uint64 = 0x4000
	FeatureSupportsArtwork                       uint64 = 0x8000
	FeatureSupportsProgress                      uint64 = 0x10000
	FeatureSupportsDaapNowPlaying                uint64 = 0x20000
	FeatureAudioFormats0                         uint64 = 0x40000
	FeatureAudioFormats1                         uint64 = 0x80000
	FeatureAudioFormats2                         uint64 = 0x100000
	FeatureAudioFormats3                         uint64 = 0x200000
	FeatureSupportsRSAAuth                       uint64 = 0x800000
	FeatureSupportsMFIAuth                       uint64 = 0x4000000
	FeatureSupportsLegacyPairing                 uint64 = 0x8000000
	FeatureHasUnifiedAdvertiserInfo              uint64 = 0x40000000
	FeatureMaybeRequiresPairing                  uint64 = 0x80000000
	FeatureIsCarPlay                             uint64 = 0x100000000
	FeatureSupportsUnifiedMediaControl           uint64 = 0x4000000000
	FeatureSupportsBufferedAudio                 uint64 = 0x10000000000
	FeatureSupportsPTP                           uint64 = 0x20000000000
	FeatureSupportsSystemPairing                 uint64 = 0x80000000000
	FeatureSupportsHomeKitPairing                uint64 = 0x400000000000
	FeatureCoreUtilsPairing                      uint64 = 0x1000000000000
	FeatureSupportsBPlistNowPlaying              uint64 = 0x4000000000000
	FeatureSupportsUnifiedPairingAndMFI          uint64 = 0x8000000000000
	FeatureSupportsSetPeersExtended              uint64 = 0x10000000000000
	FeatureSupportsTransientPairing              uint64 = 0x1080000000000
	FeatureSupportsCoreUtilsPairingAndEncryption uint64 = 0x1484000000000
	FeatureSupportsMultiRoom                     uint64 = 0x8030000000A00
)

// StatusFlags，逐字来自 AirPlayStatusFlags.cs。
const (
	StatusPinRequired                      uint32 = 8
	StatusPasswordRequired                 uint32 = 0x80
	StatusOneTimePairingRequired           uint32 = 0x200
	StatusDeviceWasSetupForHKAccessControl uint32 = 0x400
	StatusTightSyncIsGroupLeader           uint32 = 0x2000
)

// Device 一台 `_airplay._tcp` 设备（TXT 解析结果）。
type Device struct {
	Name     string
	DeviceID string // 服务名里的 MAC（归一化 AA:BB:...）
	Address  string // host:port
	Model    string

	Features               uint64 // ft / features（双段 hex：低段 | 高段<<32）
	StatusFlags            uint32 // sf / flags（hex，叠加）
	RequiresPassword       bool
	RequiresOneTimePairing bool
	PublicKey              string
	Channels               int
	SampleRate             int
	BitDepth               int

	// 设备侧编组（AirPlayDeviceData.cs:644-655）
	TightSyncUuid       string // tsid
	PublicGroupId       string // psgid
	IsPublicGroupLeader bool   // igl=1（且 psgid 非空）
}

// GroupId psgid 优先，回退 tsid（AirPlayDeviceData.cs:379）。
func (d Device) GroupId() string {
	if d.PublicGroupId != "" {
		return d.PublicGroupId
	}
	return d.TightSyncUuid
}

// IsTSGroupLeader StatusFlags 有 TightSyncIsGroupLeader 位。
func (d Device) IsTSGroupLeader() bool {
	return d.StatusFlags&StatusTightSyncIsGroupLeader != 0
}

// IsGroupLeader psgid 存在取公共组长位，否则取 TS 组长位
// （AirPlayDeviceData.cs:381-393）。
func (d Device) IsGroupLeader() bool {
	if d.PublicGroupId != "" {
		return d.IsPublicGroupLeader
	}
	if d.TightSyncUuid != "" {
		return d.IsTSGroupLeader()
	}
	return false
}

// HasFeature 是否命中某 feature 位（C# `(Features & flag) != None` 的
// 任一位命中语义）。
func (d Device) HasFeature(flag uint64) bool {
	return d.Features&flag != 0
}

// Entry 一条 mDNS 记录（与 raop.Entry 同形态；发现逻辑可注入测试）。
type Entry struct {
	Name string // 实例名（含 ._airplay._tcp.local 后缀）
	Host string
	Addr net.IP
	Port int
	TXT  []string
}

// Query mDNS 查询函数。测试注入假实现，生产走 hashicorp/mdns。
type Query func(ctx context.Context, service string, timeout time.Duration) ([]Entry, error)

// QueryMDNS 真实 mDNS 查询。
func QueryMDNS(ctx context.Context, service string, timeout time.Duration) ([]Entry, error) {
	params := mdns.DefaultParams(service)
	entries := make(chan *mdns.ServiceEntry, 32)
	params.Entries = entries
	params.Timeout = timeout
	go func() {
		defer close(entries)
		if err := mdns.QueryContext(ctx, params); err != nil {
			return
		}
	}()
	var out []Entry
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case e, ok := <-entries:
			if !ok {
				return out, nil
			}
			if e == nil || e.Host == "" {
				continue
			}
			addr := e.AddrV4
			if addr == nil {
				addr = e.AddrV6
			}
			out = append(out, Entry{Name: e.Name, Host: e.Host, Addr: addr, Port: e.Port, TXT: e.InfoFields})
		case <-time.After(time.Until(deadline)):
			return out, nil
		}
	}
	return out, nil
}

// Discover 查询 `_airplay._tcp` 并解析 + 编组合并。
func Discover(ctx context.Context, query Query, timeout time.Duration) ([]Device, error) {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	if query == nil {
		query = QueryMDNS
	}
	entries, err := query(ctx, "_airplay._tcp", timeout)
	if err != nil {
		return nil, err
	}
	var devs []Device
	for _, e := range entries {
		service := strings.ToLower(strings.TrimSuffix(e.Name, "."))
		if (!strings.HasSuffix(service, "._airplay._tcp.local") && !strings.HasSuffix(service, "._airplay._tcp")) || e.Port <= 0 || e.Port > 65535 || e.Host == "" {
			continue
		}
		if d, ok := ParseAirPlayTXT(e.TXT); ok {
			if !d.HasFeature(FeatureSupportsAirPlayAudio) {
				continue
			}
			d.Name = trimServiceSuffix(e.Name)
			if d.DeviceID == "" {
				d.DeviceID = deviceIDFromName(e.Name)
			}
			if d.DeviceID == "" {
				continue
			}
			host := e.Host
			if e.Addr != nil {
				host = e.Addr.String()
			}
			d.Address = joinHostPort(host, e.Port)
			if d.Model == "" {
				d.Model = "AirPlay"
			}
			devs = append(devs, d)
		}
	}
	return MergeGroups(devs), nil
}

// MergeGroups 设备侧编组：同 GroupId（psgid 优先）的多台合并为一台，
// 组长为代表，成员数进 Model（对齐 AirPlayDeviceData 的 GroupId/IsGroupLeader
// 语义——AirPlay 2 多房间是设备自组组，发送端只向组长播放）。
func MergeGroups(devs []Device) []Device {
	type group struct {
		rep       Device
		members   int
		hasLeader bool
	}
	order := []string{}
	groups := map[string]*group{}
	var solo []Device
	for _, d := range devs {
		gid := d.GroupId()
		if gid == "" {
			solo = append(solo, d)
			continue
		}
		g, ok := groups[gid]
		if !ok {
			g = &group{rep: d, members: 1, hasLeader: d.IsGroupLeader()}
			groups[gid] = g
			order = append(order, gid)
			continue
		}
		g.members++
		if !g.hasLeader && d.IsGroupLeader() {
			g.rep = d
			g.hasLeader = true
		}
	}
	out := make([]Device, 0, len(order)+len(solo))
	for _, gid := range order {
		g := groups[gid]
		rep := g.rep
		if g.members > 1 {
			rep.Model = rep.Model + " (" + strconv.Itoa(g.members) + ")"
		}
		out = append(out, rep)
	}
	return append(out, solo...)
}

// ParseAirPlayTXT 解析 `_airplay._tcp` TXT（对齐 AirPlayDeviceData.cs:455-660）：
//
//	ft / features → Features（双段 hex：低段 | 高段<<32）
//	sf / flags → StatusFlags（hex，两键叠加）
//	pw=true → RequiresPassword；pk → PublicKey；ch/sr/ss → 格式
//	tsid/psgid/igl → 编组字段
func ParseAirPlayTXT(txt []string) (Device, bool) {
	props := parseTXTFields(txt)
	if len(props) == 0 {
		return Device{}, false
	}
	d := Device{
		DeviceID:   normalizeDeviceID(props["deviceid"]),
		Channels:   2,
		SampleRate: 44100,
		BitDepth:   16,
	}
	if v, ok := props["ch"]; ok {
		d.Channels, _ = strconv.Atoi(v)
	}
	if v, ok := props["sr"]; ok {
		d.SampleRate, _ = strconv.Atoi(v)
	}
	if v, ok := props["ss"]; ok {
		d.BitDepth, _ = strconv.Atoi(v)
	}
	d.RequiresPassword = strings.EqualFold(props["pw"], "true")
	d.PublicKey = props["pk"]
	d.Model = props["model"]
	// et 含 MFiSAP → RequiresAuthSetup（对齐 AirPlayDeviceData.cs:566-570）
	// Features：ft 与 features 同义，双段 hex（容忍 0x 前缀——设备两种形态都发）
	for _, key := range []string{"ft", "features"} {
		if v, ok := props[key]; ok {
			parts := strings.Split(v, ",")
			if lo, err := strconv.ParseUint(trimHexPrefix(strings.TrimSpace(parts[0])), 16, 64); err == nil {
				d.Features |= lo
			}
			if len(parts) > 1 {
				if hi, err := strconv.ParseUint(trimHexPrefix(strings.TrimSpace(parts[1])), 16, 64); err == nil {
					d.Features |= hi << 32
				}
			}
		}
	}
	// StatusFlags：sf 与 flags 同义，叠加
	for _, key := range []string{"sf", "flags"} {
		if v, ok := props[key]; ok {
			parts := strings.Split(v, ",")
			if lo, err := strconv.ParseUint(trimHexPrefix(strings.TrimSpace(parts[0])), 16, 64); err == nil {
				d.StatusFlags |= uint32(lo)
			}
		}
	}
	// 编组
	d.TightSyncUuid = props["tsid"]
	d.PublicGroupId = props["psgid"]
	d.IsPublicGroupLeader = d.PublicGroupId != "" && props["igl"] == "1"
	// RequiresOneTimePairing：StatusFlags 的 OneTimePairingRequired 位
	d.RequiresOneTimePairing = d.StatusFlags&StatusOneTimePairingRequired != 0
	return d, true
}

// trimHexPrefix 剥离可选的 0x/0X 前缀（设备 TXT 两种形态都发）。
func trimHexPrefix(s string) string {
	return strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
}

func parseTXTFields(txt []string) map[string]string {
	out := make(map[string]string, len(txt))
	for _, kv := range txt {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		out[strings.ToLower(k)] = v
	}
	return out
}

func trimServiceSuffix(name string) string {
	name = decodeDNSName(name)
	if i := strings.Index(strings.ToLower(name), "._airplay._tcp"); i > 0 {
		name = name[:i]
	}
	if i := strings.Index(strings.ToLower(name), "._raop._tcp"); i > 0 {
		name = name[:i]
	}
	if id, display, ok := strings.Cut(name, "@"); ok && normalizeDeviceID(id) != "" {
		name = display
	}
	return strings.TrimSpace(name)
}

// deviceIDFromName 从服务实例名取设备 MAC（与 raop.SplitServiceName 同约定）。
func deviceIDFromName(name string) string {
	n := decodeDNSName(name)
	if i := strings.Index(strings.ToLower(n), "._airplay._tcp"); i > 0 {
		n = n[:i]
	}
	// 实例名形如 "AB12CD345678@HomePod 客厅" 或纯 MAC
	if i := strings.Index(n, "@"); i > 0 {
		n = n[:i]
	}
	return normalizeDeviceID(n)
}

func normalizeDeviceID(raw string) string {
	id := strings.NewReplacer(":", "", "-", "").Replace(strings.TrimSpace(raw))
	if len(id) != 12 {
		return ""
	}
	if _, err := hex.DecodeString(id); err != nil {
		return ""
	}
	return strings.ToLower(id)
}

func decodeDNSName(name string) string {
	var out strings.Builder
	for i := 0; i < len(name); i++ {
		if name[i] == '\\' && i+1 < len(name) {
			if i+3 < len(name) {
				if n, err := strconv.Atoi(name[i+1 : i+4]); err == nil && n >= 0 && n <= 255 {
					out.WriteByte(byte(n))
					i += 3
					continue
				}
			}
			i++
		}
		out.WriteByte(name[i])
	}
	return out.String()
}

func joinHostPort(host string, port int) string {
	if port == 0 {
		return host
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}
