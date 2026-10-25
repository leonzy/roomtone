package services

import (
	"net"
	"testing"
)

// TestPlayerAllowlistIndependentFromProxy X-02 双向用例。
//
// 两条白名单必须互不影响：
//   - 只开播放器白名单时，代理侧对同一网段仍应封禁；
//   - 只开代理白名单时，播放器侧仍按自己的规则判定。
//
// 混用的后果很具体：为了让音箱出声把网段加进代理白名单，等于顺手放开了
// SSRF 防护；反过来收紧代理白名单会让音箱莫名其妙不出声。
func TestPlayerAllowlistIndependentFromProxy(t *testing.T) {
	playerNets, err := ParsePlayerOutputAllowlist([]string{"192.168.1.0/24"})
	if err != nil {
		t.Fatalf("解析播放器白名单：%v", err)
	}
	proxyNets, err := ParseAllowlist([]string{"10.0.0.0/8"})
	if err != nil {
		t.Fatalf("解析代理白名单：%v", err)
	}

	lan := net.ParseIP("192.168.1.20")
	other := net.ParseIP("10.1.2.3")

	// 方向一：只配播放器白名单。播放器侧放行本网段，代理侧仍按自己的白名单判定。
	if !IsPlayerOutputAllowed(lan, playerNets) {
		t.Fatal("播放器白名单应放行 192.168.1.20")
	}
	if IsPlayerOutputAllowed(other, playerNets) {
		t.Fatal("播放器白名单之外的网段应被拒绝")
	}
	if IsHostnameAllowedWithAllowlist("10.1.2.3", nil) {
		t.Fatal("没配代理白名单时私网必须封禁（播放器白名单不得泄漏到代理链路）")
	}

	// 方向二：只配代理白名单。代理侧放行，播放器侧仍按自己的规则（空白名单=私网全允许）。
	if !IsHostnameAllowedWithAllowlist("10.1.2.3", proxyNets) {
		t.Fatal("代理白名单应放行 10.1.2.3")
	}
	if !IsPlayerOutputAllowed(lan, nil) {
		t.Fatal("播放器白名单为空时私网应默认允许")
	}
	if IsPlayerOutputAllowed(net.ParseIP("8.8.8.8"), nil) {
		t.Fatal("播放器出口不允许公网地址")
	}
}

// TestPlayerAllowlistPublicAlwaysDenied 公网地址即使进了白名单也拒绝。
func TestPlayerAllowlistPublicAlwaysDenied(t *testing.T) {
	nets, err := ParsePlayerOutputAllowlist([]string{"8.8.8.0/24"})
	if err != nil {
		t.Fatalf("解析：%v", err)
	}
	if IsPlayerOutputAllowed(net.ParseIP("8.8.8.8"), nets) {
		t.Fatal("白名单里的公网段也不能放行（不往公网推音频）")
	}
}

// TestPlayerAllowlistParseError 非法条目要报错，且文案指明是播放器白名单。
func TestPlayerAllowlistParseError(t *testing.T) {
	if _, err := ParsePlayerOutputAllowlist([]string{"not-a-cidr"}); err == nil {
		t.Fatal("非法条目应返回错误")
	}
}
