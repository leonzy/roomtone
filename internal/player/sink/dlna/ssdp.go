package dlna

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/ipv4"
)

// SSDP 相关常量。
const (
	// SSDPAddress 标准组播地址。
	SSDPAddress = "239.255.255.250:1900"
	// STMediaRenderer 只关心能放音的 renderer；MediaServer 只提供内容，不选。
	STMediaRenderer = "urn:schemas-upnp-org:device:MediaRenderer:1"
	// STAll 兜底查询：部分设备对精确 ST 不回，只认 ssdp:all。
	STAll = "ssdp:all"

	ssdpUserAgent = "Roomtone/1.0 UPnP/1.1 Roomtone/1.0"
)

// SSDPDevice 一条 SSDP 响应。
type SSDPDevice struct {
	ST       string
	USN      string
	UUID     string
	Location *url.URL
	Server   string
	// From 对端地址，排障用。
	From string
	// MaxAge CACHE-CONTROL 的 max-age，0 表示设备没给。
	MaxAge time.Duration
}

// SSDPOptions 发现参数。零值可用。
type SSDPOptions struct {
	// Targets 组播目标，默认 SSDPAddress；测试可指向本地端口。
	Targets []string
	// SearchTargets 查询的 ST，默认 MediaRenderer + ssdp:all。
	SearchTargets []string
	// MX 期望设备随机延迟的上界，协议建议 1-5 秒。
	MX time.Duration
	// Timeout 收包总时长。
	Timeout time.Duration
	// Retries 重发次数（UDP 会丢包）。
	Retries int
	// LocalAddrs 绑定的本机 IPv4；为空时遍历网卡。
	LocalAddrs []net.IP
	// Listen 绑定本地 socket；测试注入。
	Listen func(ip net.IP) (*net.UDPConn, error)
}

// DiscoverSSDP 发送 M-SEARCH 并收集响应。
//
// 多网卡逐个发包：Docker / 双网卡机器上只发默认路由那一张卡会漏设备。
// 响应按 USN 去重（同一设备会重复回，也会同时回 ST 和 ssdp:all 两条）。
func DiscoverSSDP(ctx context.Context, opts SSDPOptions) ([]SSDPDevice, error) {
	opts = normalizeSSDPOptions(opts)
	addrs := opts.LocalAddrs
	if addrs == nil {
		addrs = localIPv4()
	}
	if len(addrs) == 0 {
		addrs = []net.IP{net.IPv4zero}
	}

	conns := make([]*net.UDPConn, 0, len(addrs))
	for _, ip := range addrs {
		c, err := opts.Listen(ip)
		if err != nil {
			continue
		}
		conns = append(conns, c)
	}
	if len(conns) == 0 {
		return nil, errors.New("dlna: 没有可用的本机地址，无法发送 SSDP 查询")
	}
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()

	deadline := time.Now().Add(opts.Timeout)
	// deadline 必须在起读协程之前设好：stop() 会把 deadline 提前到"现在"，
	// 若在读协程里设，就会出现"刚被取消又被推回未来"的竞态，取消要等到超时。
	for _, c := range conns {
		_ = c.SetReadDeadline(deadline)
	}
	ch := make(chan SSDPDevice, 64)
	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func(c *net.UDPConn) {
			defer wg.Done()
			buf := make([]byte, 4096)
			for {
				n, from, err := c.ReadFromUDP(buf)
				if err != nil {
					return
				}
				d, err := ParseSSDPResponse(buf[:n], from)
				if err != nil || d == nil {
					continue
				}
				select {
				case ch <- *d:
				default:
				}
			}
		}(c)
	}

	// 发包单独一个协程：重发与收包并行，否则第二轮查询的响应会错过。
	go func() {
		interval := opts.Timeout / time.Duration(opts.Retries+2)
		if interval < 100*time.Millisecond {
			interval = 100 * time.Millisecond
		}
		for i := 0; i <= opts.Retries; i++ {
			for _, target := range opts.Targets {
				addr, err := net.ResolveUDPAddr("udp4", target)
				if err != nil {
					continue
				}
				for _, st := range opts.SearchTargets {
					msg := BuildMSearch(target, st, opts.MX)
					for _, c := range conns {
						_, _ = c.WriteToUDP(msg, addr)
					}
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()

	timer := time.NewTimer(opts.Timeout)
	defer timer.Stop()

	seen := make(map[string]bool)
	out := make([]SSDPDevice, 0, 8)
	stop := func() {
		// 让读协程立刻返回，否则它们会等到 deadline。
		now := time.Now()
		for _, c := range conns {
			_ = c.SetReadDeadline(now)
		}
		wg.Wait()
	}
	for {
		select {
		case <-ctx.Done():
			stop()
			return sortDevices(out), ctx.Err()
		case <-timer.C:
			stop()
			return sortDevices(out), nil
		case d := <-ch:
			key := d.USN
			if key == "" {
				key = d.Location.String()
			}
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, d)
		}
	}
}

func normalizeSSDPOptions(opts SSDPOptions) SSDPOptions {
	if len(opts.Targets) == 0 {
		opts.Targets = []string{SSDPAddress}
	}
	if len(opts.SearchTargets) == 0 {
		opts.SearchTargets = []string{STMediaRenderer, STAll}
	}
	if opts.MX <= 0 {
		opts.MX = 2 * time.Second
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 3 * time.Second
	}
	if opts.Retries < 0 {
		opts.Retries = 0
	}
	if opts.Retries == 0 {
		opts.Retries = 2
	}
	if opts.Listen == nil {
		opts.Listen = listenUDP4
	}
	return opts
}

func listenUDP4(ip net.IP) (*net.UDPConn, error) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip})
	if err != nil {
		return nil, err
	}
	// TTL=2：跨一层路由也能发现（macvlan 场景），又不至于出网。
	// net.UDPConn 没有直接的 TTL 设置，走 ipv4 控制消息。
	_ = ipv4.NewPacketConn(c).SetMulticastTTL(2)
	return c, nil
}

// localIPv4 枚举本机可用的 IPv4 单播地址。
//
// 回环也留着：容器里没有别的网卡时，同宿主的其它容器仍可通过
// 127.0.0.1 的端口映射被发现（B4-01 的兜底）。
func localIPv4() []net.IP {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []net.IP
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil {
				continue
			}
			out = append(out, ip)
		}
	}
	return out
}

// BuildMSearch 拼 M-SEARCH 请求。导出是为了 golden bytes 测试。
func BuildMSearch(host, st string, mx time.Duration) []byte {
	secs := int(mx.Round(time.Second) / time.Second)
	if secs <= 0 {
		secs = 1
	}
	var b strings.Builder
	b.WriteString("M-SEARCH * HTTP/1.1\r\n")
	b.WriteString("HOST: ")
	b.WriteString(host)
	b.WriteString("\r\n")
	b.WriteString("MAN: \"ssdp:discover\"\r\n")
	b.WriteString("MX: ")
	b.WriteString(strconv.Itoa(secs))
	b.WriteString("\r\n")
	b.WriteString("ST: ")
	b.WriteString(st)
	b.WriteString("\r\n")
	b.WriteString("USER-AGENT: ")
	b.WriteString(ssdpUserAgent)
	b.WriteString("\r\n\r\n")
	return []byte(b.String())
}

// ParseSSDPResponse 解析一条 200 OK 响应。
func ParseSSDPResponse(raw []byte, from net.Addr) (*SSDPDevice, error) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 {
		return nil, errors.New("dlna: 空的 SSDP 响应")
	}
	if status := strings.TrimSpace(lines[0]); !strings.HasPrefix(strings.ToUpper(status), "HTTP/1.1 200") {
		return nil, fmt.Errorf("dlna: 不是 SSDP 200 响应: %q", status)
	}
	headers := make(map[string]string, 8)
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	loc := headers["location"]
	if loc == "" {
		return nil, errors.New("dlna: SSDP 响应缺少 LOCATION")
	}
	u, err := url.Parse(loc)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("dlna: 无法解析 LOCATION %q", loc)
	}
	d := &SSDPDevice{
		ST:       headers["st"],
		USN:      headers["usn"],
		Location: u,
		Server:   headers["server"],
		MaxAge:   parseMaxAge(headers["cache-control"]),
	}
	d.UUID = usnUUID(d.USN)
	if from != nil {
		d.From = from.String()
	}
	return d, nil
}

// usnUUID 从 USN 里取 uuid。形如 `uuid:xxxx::urn:...`。
func usnUUID(usn string) string {
	if usn == "" {
		return ""
	}
	head, _, _ := strings.Cut(usn, "::")
	return strings.TrimPrefix(strings.TrimSpace(head), "uuid:")
}

func parseMaxAge(cc string) time.Duration {
	for _, part := range strings.Split(cc, ",") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(strings.ToLower(part), "max-age") {
			continue
		}
		_, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 0
}

// IsMediaRenderer 判断 ST 或 USN 是否指向 MediaRenderer。
func IsMediaRenderer(st, usn string) bool {
	if strings.Contains(st, "MediaRenderer") {
		return true
	}
	return strings.Contains(usn, "MediaRenderer")
}

func sortDevices(list []SSDPDevice) []SSDPDevice {
	sort.Slice(list, func(i, j int) bool {
		if list[i].UUID == list[j].UUID {
			return list[i].Location.String() < list[j].Location.String()
		}
		return list[i].UUID < list[j].UUID
	})
	return list
}
