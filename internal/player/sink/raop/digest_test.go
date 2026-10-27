package raop

import (
	"testing"
)

// 向量与 Roon ComputeHttpDigest（AirPlayClientV1.cs:3195-3199）同输入自算：
// HA1=md5hex("iTunes:"+realm+":"+password)、HA2=md5hex("OPTIONS:*")、
// resp=md5hex(HA1+":"+nonce+":"+HA2)，md5hex 为 ASCII 输入、hex 小写。
// 硬编码期望值用独立实现（Python hashlib）预计算，防实现与测试同源错位。
func TestComputeDigest(t *testing.T) {
	cases := []struct {
		realm, password, nonce, want string
	}{
		{"Music", "pw", "abc", "7716a88292c870f9ed49bdfafe0c0257"},
		{"Lounge", "s3cret!", "nonce-123", "3fa536ebd8d774feaf3a8939997f86df"},
	}
	for _, c := range cases {
		got := computeDigest(c.realm, c.password, "OPTIONS", "*", c.nonce)
		if got != c.want {
			t.Fatalf("computeDigest(%q,%q,OPTIONS,*,%q) = %s，期望 %s",
				c.realm, c.password, c.nonce, got, c.want)
		}
	}
}

// 授权头必须逐字复刻 Roon 的怪格式：uri="..." 与 response="..." 之间**无逗号**
// （AirPlayClientV1.cs:1347-1349 的 String.Format）。
func TestAuthorizationHeader(t *testing.T) {
	got := authorizationHeader("Music", "abc", "*", "7716a88292c870f9ed49bdfafe0c0257")
	want := `Digest username="iTunes", realm="Music", nonce="abc", uri="*" response="7716a88292c870f9ed49bdfafe0c0257"`
	if got != want {
		t.Fatalf("授权头不符:\n got=%q\nwant=%q", got, want)
	}
	if !containsNoCommaBeforeResponse(got) {
		t.Fatal("uri 与 response 之间不应有逗号")
	}
}

func containsNoCommaBeforeResponse(h string) bool {
	for i := 0; i+11 <= len(h); i++ {
		if h[i:i+10] == `response="` && i > 0 && h[i-1] == ',' {
			return false
		}
	}
	return true
}

func TestParseWWWAuthenticate(t *testing.T) {
	realm, nonce, ok := parseWWWAuthenticate(`Digest realm="Lounge", nonce="xyz123", qop="auth"`)
	if !ok || realm != "Lounge" || nonce != "xyz123" {
		t.Fatalf("解析失败: realm=%q nonce=%q ok=%v", realm, nonce, ok)
	}
	// 缺 nonce 也能取到 realm（对齐 Roon 的两个独立正则）。
	realm, nonce, ok = parseWWWAuthenticate(`Digest realm="Only"`)
	if !ok || realm != "Only" || nonce != "" {
		t.Fatalf("缺 nonce 的解析: realm=%q nonce=%q ok=%v", realm, nonce, ok)
	}
	if _, _, ok := parseWWWAuthenticate(`Basic`); ok {
		t.Fatal("无 realm 时应返回 ok=false")
	}
	// Roon 的正则不挑 scheme：Basic 带了 realm 也能取（对齐行为）。
	if realm, _, ok := parseWWWAuthenticate(`Basic realm="X"`); !ok || realm != "X" {
		t.Fatalf("Basic 带 realm 应可取：realm=%q ok=%v", realm, ok)
	}
}
