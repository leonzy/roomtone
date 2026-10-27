package raop

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"regexp"
)

// AirPlay 1 密码设备 = RTSP 上的 HTTP Digest 认证（RFC 2069），逐字对齐
// Roon AirPlayClientV1.cs:1320-1356, 3192-3199。
//
// 注意：这不是 FairPlay——历史上 RAOP 密码曾被误传为 FairPlay 逐包加密，
// 反编译证实 401+WWW-Authenticate 走的是标准 Digest 挑战；FairPlay/SRP6
// 属 AirPlay 2 配对路径。
//
// Roon 的两个实现细节必须逐字复刻：
//  1. 摘要按 OPTIONS 请求计算：username="iTunes"、method="OPTIONS"、uri="*"；
//     授权通过后同一头复用到后续所有请求（设备接受 uri="*"）。
//  2. 授权头里 uri="..." 与 response="..." 之间**没有逗号**（String.Format
//     模板如此），标准 Digest 头的逗号在这里会不被设备接受。

// computeDigest RFC 2069 摘要（无 qop）：HA1=MD5(user:realm:pw)、
// HA2=MD5(method:uri)、response=MD5(HA1:nonce:HA2)，hex 小写、ASCII 输入。
func computeDigest(realm, password, method, uri, nonce string) string {
	md5hex := func(s string) string {
		sum := md5.Sum([]byte(s)) //nolint:gosec // RFC 2069 指定 MD5
		return hex.EncodeToString(sum[:])
	}
	ha1 := md5hex("iTunes:" + realm + ":" + password)
	ha2 := md5hex(method + ":" + uri)
	return md5hex(ha1 + ":" + nonce + ":" + ha2)
}

// authorizationHeader Roon 怪格式：uri 与 response 之间无逗号。
func authorizationHeader(realm, nonce, uri, response string) string {
	return fmt.Sprintf(`Digest username="iTunes", realm="%s", nonce="%s", uri="%s" response="%s"`,
		realm, nonce, uri, response)
}

var (
	realmRe = regexp.MustCompile(`realm="([^"]+)"`)
	nonceRe = regexp.MustCompile(`nonce="([^"]+)"`)
)

// parseWWWAuthenticate 从 WWW-Authenticate 头取 realm 与 nonce
// （两个独立正则，对齐 AirPlayClientV1.cs:1328-1339）。
func parseWWWAuthenticate(h string) (realm, nonce string, ok bool) {
	if m := realmRe.FindStringSubmatch(h); m != nil {
		realm = m[1]
	} else {
		return "", "", false
	}
	if m := nonceRe.FindStringSubmatch(h); m != nil {
		nonce = m[1]
	}
	return realm, nonce, true
}
