package raop

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/mdns"

	"songloft/internal/player/sink"
)

// ServiceName mDNS 服务类型。AirPlay 1（RAOP）设备都在这个服务下广播。
const ServiceName = "_raop._tcp"

// DefaultDiscoverTimeout 一轮 mDNS 浏览的等待时长。
//
// mDNS 是尽力而为的：设备可能因为省电或缓存不回答第一轮查询，
// 所以调用方通常会连续扫两轮（见 internal/player/discovery）。
const DefaultDiscoverTimeout = 2 * time.Second

// Entry mDNS 查询结果的精简形态。
//
// 刻意不直接暴露 mdns.ServiceEntry：发现逻辑的测试要能在没有网络、
// 没有组播的机器上跑，构造一个 Entry 就够了。
type Entry struct {
	// Name 服务实例名，形如 `AABBCCDDEEFF@Living Room._raop._tcp.local.`。
	Name string
	Host string
	Addr net.IP
	Port int
	TXT  []string
}

// Found 一台可用的 RAOP 设备。
type Found struct {
	// DeviceID 服务名里的设备 id（MAC 十六进制），RTSP 路径要用。
	DeviceID string
	// Name 用户可见名称（服务名 @ 之后的部分）。
	Name string
	// Model TXT 的 md 字段。
	Model string
	// Address 设备 RTSP 地址 host:port。
	Address string
	Caps    sink.Caps
	// Transport TXT 的 tp 字段（UDP / TCP）。
	Transport string
}

// Query mDNS 查询函数。测试注入假实现，生产走 hashicorp/mdns。
type Query func(ctx context.Context, service string, timeout time.Duration) ([]Entry, error)

// Discover 浏览 _raop._tcp，返回去重后的设备列表（按名称排序）。
//
// query 为 nil 时用真实 mDNS。单台设备解析失败只跳过它，不整体报错：
// 局域网里混着打印机、电视，脏数据是常态。
func Discover(ctx context.Context, query Query, timeout time.Duration) ([]Found, error) {
	if timeout <= 0 {
		timeout = DefaultDiscoverTimeout
	}
	if query == nil {
		query = QueryMDNS
	}
	entries, err := query(ctx, ServiceName, timeout)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(entries))
	out := make([]Found, 0, len(entries))
	for _, e := range entries {
		f, ok := EntryToFound(e)
		if !ok || seen[f.DeviceID] {
			continue
		}
		seen[f.DeviceID] = true
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name == out[j].Name {
			return out[i].DeviceID < out[j].DeviceID
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// QueryMDNS 真实 mDNS 查询。
func QueryMDNS(ctx context.Context, service string, timeout time.Duration) ([]Entry, error) {
	params := mdns.DefaultParams(service)
	params.Timeout = timeout
	entries := make(chan *mdns.ServiceEntry, 32)
	params.Entries = entries

	collected := make(chan []Entry, 1)
	go func() {
		var out []Entry
		for e := range entries {
			out = append(out, Entry{
				Name: e.Name,
				Host: e.Host,
				Addr: e.AddrV4,
				Port: e.Port,
				TXT:  e.InfoFields,
			})
		}
		collected <- out
	}()

	err := mdns.QueryContext(ctx, params)
	close(entries)
	out := <-collected
	return out, err
}

// EntryToFound 把一条 mDNS 记录转成设备信息。
//
// 缺地址或缺端口直接判为不可用：只有 SRV 没有 A 记录的条目在
// hashicorp/mdns 里不会 complete，但注入的测试数据可能是残的。
func EntryToFound(e Entry) (Found, bool) {
	deviceID, name := SplitServiceName(e.Name)
	if deviceID == "" || e.Port <= 0 || e.Addr == nil {
		return Found{}, false
	}
	txt := ParseTXTFields(e.TXT)
	caps := ParseTXT(e.TXT)
	caps.DeviceID = deviceID
	caps.PublicKey = NormalizePublicKey(txt["pk"])
	caps.NeedsPassword = txt["pw"] == "true"
	if name == "" {
		name = deviceID
	}
	return Found{
		DeviceID:  deviceID,
		Name:      name,
		Model:     txt["md"],
		Address:   net.JoinHostPort(e.Addr.String(), strconv.Itoa(e.Port)),
		Caps:      caps,
		Transport: txt["tp"],
	}, true
}

// SplitServiceName 拆 `AABBCCDDEEFF@Living Room._raop._tcp.local.`。
//
// 返回设备 id（大写、去掉分隔符）与显示名。没有 @ 的条目按
// "整段都是 id"处理——有些老设备只广播 MAC。
func SplitServiceName(name string) (deviceID, display string) {
	name = strings.TrimSuffix(strings.TrimSpace(name), ".")
	for _, suffix := range []string{"._raop._tcp.local", "._raop._tcp", "._airplay._tcp.local"} {
		if strings.HasSuffix(strings.ToLower(name), suffix) {
			name = name[:len(name)-len(suffix)]
			break
		}
	}
	id, rest, hasAt := strings.Cut(name, "@")
	id = normalizeDeviceID(id)
	if !hasAt {
		return id, ""
	}
	return id, strings.TrimSpace(rest)
}

func normalizeDeviceID(raw string) string {
	raw = strings.TrimSpace(raw)
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'z':
			b.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r - 'A' + 'a')
		case r == ':' || r == '-':
			// 分隔符丢掉：RTSP 路径里的 id 是纯十六进制
		default:
			// 其它字符说明这压根不是 MAC（比如 `_raop._tcp` 之外的设备）
			return ""
		}
	}
	id := b.String()
	if len(id) != 12 {
		return ""
	}
	return id
}

// ParseTXTFields 把 TXT 记录摊平成 map。重复键取第一个（mDNS 不允许重复，
// 但脏设备会发）。
func ParseTXTFields(txt []string) map[string]string {
	out := make(map[string]string, len(txt))
	for _, kv := range txt {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if _, dup := out[k]; dup {
			continue
		}
		out[k] = strings.TrimSpace(v)
	}
	return out
}

// NormalizePublicKey 把 TXT 里的 pk 统一成 PEM。
//
// AirPlay 广播的 pk 是 base64 编码的 DER（SubjectPublicKeyInfo）；
// sink 侧按 PEM 解析，所以在入库前转一次。已经是 PEM 的原样返回，
// 解析失败返回空串——设备不可用时宁可明文也不要带着坏公钥起播。
func NormalizePublicKey(pk string) string {
	pk = strings.TrimSpace(pk)
	if pk == "" {
		return ""
	}
	if strings.Contains(pk, "-----BEGIN") {
		return pk
	}
	der, err := base64.StdEncoding.DecodeString(pk)
	if err != nil {
		return ""
	}
	key, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		if k1, err1 := x509.ParsePKCS1PublicKey(der); err1 == nil {
			key = k1
		} else {
			return ""
		}
	}
	if _, ok := key.(*x509.Certificate); ok {
		return ""
	}
	body, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return ""
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: body}))
}
