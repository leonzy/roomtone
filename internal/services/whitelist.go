package services

import (
	"fmt"
	"net"
	"strings"
)

// IsHostnameAllowed 检查域名是否允许代理
// 采用内网封禁策略：阻止访问内网地址以防止 SSRF，允许所有外网域名。
// 等价于 IsHostnameAllowedWithAllowlist(hostname, nil)：不带白名单时私网一律拒绝。
func IsHostnameAllowed(hostname string) bool {
	return IsHostnameAllowedWithAllowlist(hostname, nil)
}

// IsHostnameAllowedWithAllowlist 在内网封禁策略基础上，允许命中 allowlist 的私网地址通过。
//
// 判定规则：
//   - 外网 IP：恒放行
//   - 私网 / 回环 / 链路本地 IP：仅当被 allowlist 中某条网段覆盖时放行，否则整体拒绝
//   - 空 hostname：拒绝
//   - DNS 解析失败：放行（交由后续 HTTP 请求自行报错，与历史行为一致）
//
// allowlist 为 nil / 空时退化为纯内网封禁（历史行为）。
func IsHostnameAllowedWithAllowlist(hostname string, allowlist []*net.IPNet) bool {
	hostname = strings.ToLower(strings.TrimSpace(hostname))

	// 阻止明显的内网主机名（白名单仅按 IP/CIDR 匹配，无法覆盖这些名字）
	if hostname == "" || hostname == "localhost" || strings.HasSuffix(hostname, ".local") {
		return false
	}

	// DNS 解析域名（字面量 IP 也走此函数，返回单元素切片），检查解析结果
	ips, err := net.LookupIP(hostname)
	if err != nil {
		// 解析失败也放行，交由后续 HTTP 请求自行报错
		return true
	}

	for _, ip := range ips {
		if isPrivateIP(ip) && !allowlistContains(allowlist, ip) {
			return false
		}
	}

	return true
}

// ParseAllowlist 将文本白名单条目解析为 CIDR 网段列表。
// 支持两种格式：
//   - CIDR 网段：如 "192.168.1.0/24"、"fd00::/8"
//   - 单个 IP：如 "192.168.1.100"、"::1"（自动补 /32 或 /128）
//
// 空字符串条目被忽略；任一条目非法时返回 error（供 PUT 校验拒绝）。
func ParseAllowlist(entries []string) ([]*net.IPNet, error) {
	nets := make([]*net.IPNet, 0, len(entries))
	for _, raw := range entries {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}

		// 优先按 CIDR 解析
		if _, ipNet, err := net.ParseCIDR(entry); err == nil {
			nets = append(nets, ipNet)
			continue
		}

		// 退回按单个 IP 解析，补足全掩码
		if ip := net.ParseIP(entry); ip != nil {
			bits := 32
			if ip.To4() == nil {
				bits = 128
			}
			nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}

		return nil, fmt.Errorf("非法的白名单条目: %q（需为 IP 或 CIDR 网段）", entry)
	}
	return nets, nil
}

// allowlistContains 判断 ip 是否被 allowlist 中任一网段覆盖。
func allowlistContains(allowlist []*net.IPNet, ip net.IP) bool {
	for _, n := range allowlist {
		if n != nil && n.Contains(ip) {
			return true
		}
	}
	return false
}

// isPrivateIP 检查 IP 是否为内网/保留地址
func isPrivateIP(ip net.IP) bool {
	// 回环地址: 127.0.0.0/8, ::1
	if ip.IsLoopback() {
		return true
	}
	// 私有地址: 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, fc00::/7
	if ip.IsPrivate() {
		return true
	}
	// 链路本地: 169.254.0.0/16, fe80::/10
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	// 未指定地址: 0.0.0.0, ::
	if ip.IsUnspecified() {
		return true
	}
	return false
}

// --- 播放器出口白名单（X-02） ---
//
// 与上面的代理白名单**互不影响**：代理白名单控制"服务端出站 HTTP 请求能去哪"，
// 播放器出口白名单控制"能往哪些网段推音频"。两者混用会出事：
// 为了让音箱能连，把 192.168.0.0/16 加进代理白名单，等于顺手放开了 SSRF 防护；
// 反过来，收紧代理白名单会莫名其妙让音箱不出声。
//
// 所以这里单独一套解析与判定，配置键也不同（player_output_allowlist）。

// IsPlayerOutputAllowed 判断播放器出口目标是否允许。
//
// 语义与代理白名单刻意不同：音箱本来就在局域网里，**私网默认允许**；
// 白名单为空表示"不限制"，非空时则变成"只允许这些网段"（收敛到指定音箱网段）。
// 公网地址一律拒绝——本项目不往公网推音频（那会把家里音箱变成开放代理）。
func IsPlayerOutputAllowed(ip net.IP, allowlist []*net.IPNet) bool {
	if ip == nil {
		return false
	}
	if !isPrivateIP(ip) && !ip.IsLoopback() {
		return false
	}
	if len(allowlist) == 0 {
		return true
	}
	return allowlistContains(allowlist, ip)
}

// ParsePlayerOutputAllowlist 解析播放器出口白名单（复用 CIDR 语法，独立入口）。
//
// 单独一个函数而不是直接调 ParseAllowlist：两条链路的错误文案要能区分，
// 否则用户改错了地方会收到"代理白名单格式错误"这种误导性提示。
func ParsePlayerOutputAllowlist(entries []string) ([]*net.IPNet, error) {
	nets, err := ParseAllowlist(entries)
	if err != nil {
		return nil, fmt.Errorf("播放器出口白名单格式错误：%w", err)
	}
	return nets, nil
}
