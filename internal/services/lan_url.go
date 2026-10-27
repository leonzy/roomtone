package services

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"
)

// LANURLError 解析不出局域网地址。
var LANURLError = errors.New("cannot resolve a LAN reachable address")

// LANURLResolver 为局域网设备（DLNA renderer / AirPlay 音箱）解析本机的可达地址。
//
// 为什么不能复用 InternalURLResolver：那个恒返回 127.0.0.1，
// 只适合后端自己访问自己。把 http://127.0.0.1:58091/... 交给音箱，
// 音箱会去拉**它自己**的回环，永远拿不到音频。
//
// 做法：向目标设备地址发一个不产生流量的 UDP "连接"（connect 不发包，
// 只让内核选路），从 LocalAddr 拿到本机在该路由上的出口 IP。
// 这比枚举网卡猜一个私网地址可靠：多网卡 / VPN / 容器网络下都能选对。
type LANURLResolver struct {
	serverPort int
	// scheme 默认 http（局域网内不上 TLS）。
	scheme string
	// probe 目标端口无关紧要，只要路由与设备一致。
	timeout time.Duration
}

// NewLANURLResolver 创建解析器。
func NewLANURLResolver(serverPort int) *LANURLResolver {
	return &LANURLResolver{serverPort: serverPort, scheme: "http", timeout: 2 * time.Second}
}

// SetScheme 覆盖协议（测试或反代场景）。
func (r *LANURLResolver) SetScheme(scheme string) {
	if scheme != "" {
		r.scheme = scheme
	}
}

// Resolve 返回 http://<出口IP>:<port><path>?<query>，path 必须以 "/" 开头。
//
// target 是对端设备地址（IP 或 host:port）。解析失败返回 LANURLError，
// 调用方应把它翻译成"当前网络环境不支持音箱推送"而不是 500。
func (r *LANURLResolver) Resolve(ctx context.Context, target, path, query string) (string, error) {
	if r == nil || r.serverPort <= 0 {
		return "", fmt.Errorf("%w: server port not configured", LANURLError)
	}
	ip, err := OutboundIP(ctx, target, r.timeout)
	if err != nil {
		return "", err
	}
	u := url.URL{Scheme: r.scheme, Host: net.JoinHostPort(ip, strconv.Itoa(r.serverPort)), Path: path, RawQuery: query}
	return u.String(), nil
}

// OutboundIP 选出访问 target 时使用的本机 IP。
//
// UDP connect 不会发任何包，但会让内核按目标地址做路由决策，
// 因此拿到的就是设备能回连的那个地址。
func OutboundIP(ctx context.Context, target string, timeout time.Duration) (string, error) {
	if target == "" {
		return "", fmt.Errorf("%w: empty target", LANURLError)
	}
	host := target
	if u, err := url.Parse(target); err == nil && u.Hostname() != "" {
		host = u.Hostname()
	}
	if h, _, err := net.SplitHostPort(target); err == nil {
		host = h
	}
	if net.ParseIP(host) == nil {
		// 主机名先解析成 IP，避免 UDP 拨号时再做一次 DNS。
		addrs, err := net.DefaultResolver.LookupHost(ctx, host)
		if err != nil || len(addrs) == 0 {
			return "", fmt.Errorf("%w: resolve %s: %v", LANURLError, host, err)
		}
		host = addrs[0]
	}
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "udp", net.JoinHostPort(host, "9"))
	if err != nil {
		return "", fmt.Errorf("%w: dial %s: %v", LANURLError, host, err)
	}
	defer conn.Close()
	local, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || local.IP == nil || local.IP.IsUnspecified() {
		return "", fmt.Errorf("%w: no local address for %s", LANURLError, host)
	}
	ip := local.IP
	// 目标是 IPv4 时优先给 IPv4：多数 DLNA 设备不支持 IPv6。
	if ip.To4() != nil {
		return ip.To4().String(), nil
	}
	return ip.String(), nil
}
