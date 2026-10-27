package raop2

import (
	"bufio"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha1"
	"fmt"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"songloft/internal/player/sink/airplay"
)

// fakePairDevice 假设备端：按反编译的**设备侧**行为独立实现三路配对应答，
// 与客户端实现互验（不是 mock 同一实现）。
//
// 设备侧 SRP（标准 SRP-6a 服务端）：
//
//	x = H(salt ‖ H(id ‖ ':' ‖ pin))；v = g^x；B = (k·v + g^b) mod N
//	收到 A 后：S = (A · v^u)^b mod N；按各路径派生 SessionKey 并验 proof。
type fakePairDevice struct {
	ln net.Listener

	pin string // 设备侧密码（pair-setup 的 "3939"/密码、pair-setup-pin 的 pin）

	mu     sync.Mutex
	reqs   []pairReq // 请求记录（method/path/headers/body）
	failAt string    // 在该 path 上回 TLV Error（pair-setup 错误路径用）

	// 设备侧长期 ed25519 身份（pair-verify 签名用）
	devPriv ed25519.PrivateKey
	devPub  ed25519.PublicKey

	// route 请求路由（字段化便于测试劫持：加密往返测试替换为解密版）
	route func(method, path string, body []byte, failAt string) (int, string, []byte)

	// 跨请求状态
	setupSRP  *devSRP // pair-setup #1→#2→#3
	pinSRP    *devSRP // pair-setup-pin #1→#2→#3
	verifyEph *ecdh.PrivateKey
	verifyC2E []byte // pair-verify #1 里客户端的临时公钥
	clientPub []byte // 客户端长期公钥（#1/#3 里取到）

	t *testing.T
}

type pairReq struct {
	method, path string
	headers      map[string]string
	body         []byte
}

func newFakePairDevice(t *testing.T, pin string) *fakePairDevice {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	d := &fakePairDevice{ln: ln, pin: pin, devPriv: priv, devPub: pub, t: t}
	d.route = d.routeImpl
	go d.serve()
	t.Cleanup(func() { ln.Close() })
	return d
}

func (d *fakePairDevice) addr() string { return d.ln.Addr().String() }

func (d *fakePairDevice) requests() []pairReq {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]pairReq(nil), d.reqs...)
}

func (d *fakePairDevice) serve() {
	for {
		conn, err := d.ln.Accept()
		if err != nil {
			return
		}
		go d.handle(conn)
	}
}

func (d *fakePairDevice) handle(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		parts := strings.SplitN(strings.TrimSpace(line), " ", 3)
		if len(parts) < 3 {
			return
		}
		method, path := parts[0], parts[1]
		headers := map[string]string{}
		for {
			h, err := br.ReadString('\n')
			if err != nil {
				return
			}
			h = strings.TrimRight(h, "\r\n")
			if h == "" {
				break
			}
			k, v, ok := strings.Cut(h, ":")
			if ok {
				headers[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
			}
		}
		body := []byte{}
		if n := headers["content-length"]; n != "" {
			var size int
			fmt.Sscanf(n, "%d", &size)
			buf := make([]byte, size)
			if _, err := io.ReadFull(br, buf); err != nil {
				return
			}
			body = buf
		}
		d.mu.Lock()
		d.reqs = append(d.reqs, pairReq{method: method, path: path, headers: headers, body: body})
		failAt := d.failAt
		route := d.route
		d.mu.Unlock()

		status, ctype, respBody := route(method, path, body, failAt)
		extra := ""
		if ctype != "" {
			extra = fmt.Sprintf("Content-Type: %s\r\nContent-Length: %d\r\n", ctype, len(respBody))
		}
		if _, err := fmt.Fprintf(conn, "RTSP/1.0 %d OK\r\nCSeq: %s\r\n%s\r\n", status, headers["cseq"], extra); err == nil && len(respBody) > 0 {
			_, _ = conn.Write(respBody)
		}
	}
}

// --- 设备侧 SRP（三条路径共用的小工具）---

func devSHA1(b []byte) []byte { s := sha1.Sum(b); return s[:] }

func devPaddedPair(n1, n2, N *big.Int, h func([]byte) []byte) []byte {
	L := (N.BitLen() + 7) / 8
	pad := func(n *big.Int) []byte {
		b := n.Bytes()
		for len(b) < L {
			b = append([]byte{0}, b...)
		}
		return b
	}
	return h(append(pad(n1), pad(n2)...))
}

// devSRP 设备侧 SRP-6a 会话。
type devSRP struct {
	N, g, v, b, B *big.Int
	salt          []byte
	hash          func([]byte) []byte // SHA1 / SHA512
	sessionKey    []byte
}

func newDevSRP(id, pin string, N, g *big.Int, h func([]byte) []byte) *devSRP {
	salt := make([]byte, 16)
	rand.Read(salt)
	// x = H(salt ‖ H(id ‖ ':' ‖ pin))
	inner := h(append(append([]byte(id), 0x3a), pin...))
	x := new(big.Int).SetBytes(h(append(append([]byte{}, salt...), inner...)))
	v := new(big.Int).Exp(g, x, N)
	// b ∈ [2^255, 2^256)
	bBytes := make([]byte, 32)
	rand.Read(bBytes)
	bBytes[0] |= 0x80
	b := new(big.Int).SetBytes(bBytes)
	k := new(big.Int).SetBytes(devPaddedPair(N, g, N, h))
	// B = (k·v + g^b) mod N
	B := new(big.Int).Add(new(big.Int).Mul(k, v), new(big.Int).Exp(g, b, N))
	B.Mod(B, N)
	return &devSRP{N: N, g: g, v: v, b: b, B: B, salt: salt, hash: h}
}

// finish 收到客户端 A：S = (A · v^u)^b，派生 SessionKey。
func (s *devSRP) finish(A []byte, sessionKey func(S *big.Int) []byte) {
	a := new(big.Int).SetBytes(A)
	u := new(big.Int).SetBytes(devPaddedPair(a, s.B, s.N, s.hash))
	S := new(big.Int).Exp(new(big.Int).Mul(a, new(big.Int).Exp(s.v, u, s.N)), s.b, s.N)
	s.sessionKey = sessionKey(S)
}

func (s *devSRP) hXOR() []byte {
	hN := s.hash(s.N.Bytes())
	hG := s.hash(s.g.Bytes())
	out := make([]byte, len(hN))
	for i := range hN {
		out[i] = hN[i] ^ hG[i]
	}
	return out
}

func (s *devSRP) proof(id string, A, B []byte) []byte {
	hID := s.hash([]byte(id))
	return s.hash(concat(s.hXOR(), hID, s.salt, A, B, s.sessionKey))
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// --- 三条路径的设备侧路由 ---

func (d *fakePairDevice) routeImpl(method, path string, body []byte, failAt string) (int, string, []byte) {
	if method != "POST" {
		return 404, "", nil
	}
	if path == failAt {
		var t TLV
		t.Add(TLVState, []byte{2})
		t.Add(TLVError, []byte{4})
		return 200, "application/octet-stream", t.Encode()
	}
	switch path {
	case "/pair-setup":
		return d.handlePairSetup(body)
	case "/pair-setup-pin":
		return d.handlePairSetupPin(body)
	case "/pair-verify":
		return d.handlePairVerify(body)
	}
	return 404, "", nil
}

func (d *fakePairDevice) handlePairSetup(body []byte) (int, string, []byte) {
	t, err := DecodeTLV(body)
	if err != nil {
		d.t.Fatalf("设备侧 pair-setup TLV 解码: %v", err)
	}
	state := t.Get(TLVState)
	switch {
	case len(state) == 1 && state[0] == 1:
		// PS-Msg01 → PS-Msg02
		d.setupSRP = newDevSRP("Pair-Setup", d.pin, srpN2, srpG2, sha512Sum)
		var resp TLV
		resp.Add(TLVState, []byte{2})
		resp.Add(TLVPublicKey, d.setupSRP.B.Bytes())
		resp.Add(TLVSalt, d.setupSRP.salt)
		return 200, "application/octet-stream", resp.Encode()
	case len(state) == 1 && state[0] == 3:
		// PS-Msg03：验 proof → PS-Msg04
		srp := d.setupSRP
		A, M := t.Get(TLVPublicKey), t.Get(TLVProof)
		srp.finish(A, func(S *big.Int) []byte { return sha512Sum(S.Bytes()) })
		want := srp.proof("Pair-Setup", A, srp.B.Bytes())
		if string(M) != string(want) {
			d.t.Fatalf("设备侧 SRP proof 验证失败（客户端与服务端实现不一致）")
		}
		var resp TLV
		resp.Add(TLVState, []byte{4})
		return 200, "application/octet-stream", resp.Encode()
	case len(state) == 1 && state[0] == 5:
		// PS-Msg05：解密 + 验签 → PS-Msg06
		srp := d.setupSRP
		ct := t.Get(TLVEncryptedDataWithTag)
		encKey := hkdfSHA512(srp.sessionKey, []byte("Pair-Setup-Encrypt-Salt"), []byte("Pair-Setup-Encrypt-Info"), 32)
		nonce := make([]byte, 12)
		copy(nonce[4:], []byte("PS-Msg05"))
		pt, err := chachaOpen(encKey, nonce, ct)
		if err != nil {
			d.t.Fatalf("设备侧 PS-Msg05 解密失败: %v", err)
		}
		inner, err := DecodeTLV(pt)
		if err != nil {
			d.t.Fatalf("设备侧 PS-Msg05 TLV 解码: %v", err)
		}
		deviceX := hkdfSHA512(srp.sessionKey, []byte("Pair-Setup-Controller-Sign-Salt"), []byte("Pair-Setup-Controller-Sign-Info"), 32)
		clientPub := inner.Get(TLVPublicKey)
		info := concat(deviceX, inner.Get(TLVIdentifier), clientPub)
		if !ed25519.Verify(clientPub, info, inner.Get(TLVSignature)) {
			d.t.Fatalf("设备侧 PS-Msg05 验签失败")
		}
		d.clientPub = clientPub
		var resp TLV
		resp.Add(TLVState, []byte{6})
		return 200, "application/octet-stream", resp.Encode()
	}
	d.t.Fatalf("设备侧 pair-setup 未知状态: %v", state)
	return 0, "", nil
}

func (d *fakePairDevice) handlePairSetupPin(body []byte) (int, string, []byte) {
	m, err := airplay.DecodeBplistAny(body)
	if err != nil {
		d.t.Fatalf("设备侧 pair-setup-pin bplist 解码: %v", err)
	}
	if _, ok := m["method"]; ok {
		// #1 {method, user} → {pk, salt}
		if m["method"] != "pin" {
			d.t.Fatalf("method = %v，期望 pin", m["method"])
		}
		// Airplay1：N=srpN1（2048 群）、g=2、SHA-1
		d.pinSRP = newDevSRP(m["user"].(string), d.pin, srpN1, big.NewInt(2), devSHA1)
		resp, _ := airplay.EncodeBplist(map[string]any{"pk": d.pinSRP.B.Bytes(), "salt": d.pinSRP.salt})
		return 200, "application/x-apple-binary-plist", resp
	}
	if _, ok := m["proof"]; ok {
		// #2 {pk, proof}：验 proof → {}
		srp := d.pinSRP
		A, M := m["pk"].([]byte), m["proof"].([]byte)
		srp.finish(A, func(S *big.Int) []byte {
			sb := S.Bytes()
			sk1 := sha1.Sum(append(append([]byte{}, sb...), 0, 0, 0, 0))
			sk2 := sha1.Sum(append(append([]byte{}, sb...), 0, 0, 0, 1))
			return append(sk1[:], sk2[:]...)
		})
		want := srp.proof(clientDeviceIDDefault, A, srp.B.Bytes())
		if string(M) != string(want) {
			d.t.Fatalf("设备侧 pair-setup-pin SRP proof 验证失败")
		}
		resp, _ := airplay.EncodeBplist(map[string]any{})
		return 200, "application/x-apple-binary-plist", resp
	}
	if _, ok := m["epk"]; ok {
		// #3 {epk, authTag}：AES-GCM 解出 32B 公钥
		srp := d.pinSRP
		key := sha512Sum(append([]byte("Pair-Setup-AES-Key"), srp.sessionKey...))[:16]
		iv := append([]byte(nil), sha512Sum(append([]byte("Pair-Setup-AES-IV"), srp.sessionKey...))[:16]...)
		iv[15]++ // Roon 怪癖：末字节 +1 无进位
		ct := append(append([]byte{}, m["epk"].([]byte)...), m["authTag"].([]byte)...)
		pt, err := gcmOpen(key, iv, ct)
		if err != nil {
			d.t.Fatalf("设备侧 pair-setup-pin #3 GCM 解密失败: %v", err)
		}
		if len(pt) != 32 {
			d.t.Fatalf("设备侧解出 %d 字节，期望 32（公钥）", len(pt))
		}
		d.clientPub = pt
		resp, _ := airplay.EncodeBplist(map[string]any{})
		return 200, "application/x-apple-binary-plist", resp
	}
	d.t.Fatalf("设备侧 pair-setup-pin 未知报文: %v", m)
	return 0, "", nil
}

func (d *fakePairDevice) handlePairVerify(body []byte) (int, string, []byte) {
	if len(body) == 4+32+32 && body[0] == 1 {
		// #1 [1,0,0,0][eph 32][clientPub 32]
		clientEphPub, clientPub := body[4:36], body[36:68]
		ephPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
		if err != nil {
			d.t.Fatal(err)
		}
		d.verifyEph, d.verifyC2E, d.clientPub = ephPriv, clientEphPub, clientPub
		// 设备签名（Roon 客户端不验签，但保持形态完整）
		sig := ed25519.Sign(d.devPriv, concat(ephPriv.PublicKey().Bytes(), clientEphPub))
		return 200, "application/octet-stream", concat(ephPriv.PublicKey().Bytes(), sig)
	}
	if len(body) == 4+64 {
		// #2 [0,0,0,0][ct 64]：还原完整 128B 计数器流解出 mySig 并验签
		ct := body[4:]
		shared, err := d.verifyEph.ECDH(mustX25519Pub(d.verifyC2E))
		if err != nil {
			d.t.Fatal(err)
		}
		key := sha512Sum(append([]byte("Pair-Verify-AES-Key"), shared...))[:16]
		iv := sha512Sum(append([]byte("Pair-Verify-AES-IV"), shared...))[:16]
		// 客户端加密的是 deviceSig ‖ mySig（128B）后取 [64:128]；
		// 设备侧用自己的 sig 补齐前 64B 还原整条计数器流。
		devSig := ed25519.Sign(d.devPriv, concat(d.verifyEph.PublicKey().Bytes(), d.verifyC2E))
		full := aesECBCounterXOR(key, iv, concat(devSig, ct))
		mySig := full[64:128]
		if !ed25519.Verify(d.clientPub, concat(d.verifyC2E, d.verifyEph.PublicKey().Bytes()), mySig) {
			d.t.Fatalf("设备侧 pair-verify #2 验签失败")
		}
		return 200, "application/octet-stream", nil
	}
	d.t.Fatalf("设备侧 pair-verify 未知报文（%d 字节）", len(body))
	return 0, "", nil
}

func mustX25519Pub(b []byte) *ecdh.PublicKey {
	k, err := ecdh.X25519().NewPublicKey(b)
	if err != nil {
		panic(err)
	}
	return k
}

func chachaOpen(key, nonce, ctAndTag []byte) ([]byte, error) {
	aead, err := chachaNew(key)
	if err != nil {
		return nil, err
	}
	return aead.Open(nil, nonce, ctAndTag, nil)
}

// --- 测试 ---

func dialPairing(t *testing.T, d *fakePairDevice) (*RTSPClient, *Pairing) {
	t.Helper()
	cl, err := Dial(context.Background(), d.addr(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	return cl, NewPairing(cl, nil)
}

func TestPairSetupThreeRounds(t *testing.T) {
	d := newFakePairDevice(t, "3939")
	cl, p := dialPairing(t, d)

	if err := p.PairSetup(context.Background(), "3939", true); err != nil {
		t.Fatalf("PairSetup: %v", err)
	}
	reqs := d.requests()
	if len(reqs) != 3 {
		t.Fatalf("期望 3 轮请求，实际 %d", len(reqs))
	}
	for i, r := range reqs {
		if r.method != "POST" || r.path != "/pair-setup" {
			t.Fatalf("第 %d 轮 = %s %s，期望 POST /pair-setup", i+1, r.method, r.path)
		}
		// #1/#2 用 transient 的 "4"，#3 恒 "3"（AirPlayClientV2.cs:3581）
		wantHKP := "4"
		if i == 2 {
			wantHKP = "3"
		}
		if r.headers["x-apple-hkp"] != wantHKP {
			t.Fatalf("第 %d 轮 X-Apple-HKP = %q，期望 %q", i+1, r.headers["x-apple-hkp"], wantHKP)
		}
		if r.headers["x-apple-client-name"] != "Roon" {
			t.Fatalf("第 %d 轮缺少 X-Apple-Client-Name: Roon", i+1)
		}
		if r.headers["content-type"] != "application/octet-stream" {
			t.Fatalf("第 %d 轮 Content-Type = %q", i+1, r.headers["content-type"])
		}
	}
	// transient 完成后加密 RTSP 已启用
	if !cl.EncryptionEnabled() {
		t.Fatal("transient pair-setup 完成后应启用加密 RTSP")
	}
	// 状态机轮次：State 1 → 3 → 5
	for i, want := range []byte{1, 3, 5} {
		tl, err := DecodeTLV(reqs[i].body)
		if err != nil {
			t.Fatal(err)
		}
		if got := tl.Get(TLVState); len(got) != 1 || got[0] != want {
			t.Fatalf("第 %d 轮 State = %v，期望 [%d]", i+1, got, want)
		}
	}
	// transient 的 #1 带 Flags=[16]
	tl, _ := DecodeTLV(reqs[0].body)
	if got := tl.Get(TLVFlags); len(got) != 1 || got[0] != 16 {
		t.Fatalf("transient #1 Flags = %v，期望 [16]", got)
	}
}

func TestPairSetupDeviceError(t *testing.T) {
	d := newFakePairDevice(t, "3939")
	d.mu.Lock()
	d.failAt = "/pair-setup"
	d.mu.Unlock()
	_, p := dialPairing(t, d)
	if err := p.PairSetup(context.Background(), "3939", true); err == nil {
		t.Fatal("设备回 Error 时应失败")
	}
}

func TestPairSetupPinThreeRounds(t *testing.T) {
	d := newFakePairDevice(t, "1234")
	_, p := dialPairing(t, d)

	if err := p.PairSetupPin(context.Background(), "1234"); err != nil {
		t.Fatalf("PairSetupPin: %v", err)
	}
	reqs := d.requests()
	if len(reqs) != 3 {
		t.Fatalf("期望 3 轮请求，实际 %d", len(reqs))
	}
	for i, r := range reqs {
		if r.method != "POST" || r.path != "/pair-setup-pin" {
			t.Fatalf("第 %d 轮 = %s %s", i+1, r.method, r.path)
		}
		if r.headers["content-type"] != "application/x-apple-binary-plist" {
			t.Fatalf("第 %d 轮 Content-Type = %q", i+1, r.headers["content-type"])
		}
	}
}

func TestPairVerifyTwoRounds(t *testing.T) {
	d := newFakePairDevice(t, "1234")
	_, p := dialPairing(t, d)

	if err := p.PairVerify(context.Background()); err != nil {
		t.Fatalf("PairVerify: %v", err)
	}
	reqs := d.requests()
	if len(reqs) != 2 {
		t.Fatalf("期望 2 轮请求，实际 %d", len(reqs))
	}
	for i, r := range reqs {
		if r.method != "POST" || r.path != "/pair-verify" {
			t.Fatalf("第 %d 轮 = %s %s", i+1, r.method, r.path)
		}
		if r.headers["content-type"] != "application/octet-stream" {
			t.Fatalf("第 %d 轮 Content-Type = %q", i+1, r.headers["content-type"])
		}
	}
	// #1 body = [1,0,0,0][eph 32][pub 32]
	if len(reqs[0].body) != 68 || reqs[0].body[0] != 1 {
		t.Fatalf("#1 body 形态不符：len=%d b0=%d", len(reqs[0].body), reqs[0].body[0])
	}
	// #2 body = [4B 0][ct 64]
	if len(reqs[1].body) != 68 {
		t.Fatalf("#2 body 长度 = %d，期望 68", len(reqs[1].body))
	}
	for i, b := range reqs[1].body[:4] {
		if b != 0 {
			t.Fatalf("#2 body 前 4 字节应为零，第 %d 字节 = %d", i, b)
		}
	}
}

func TestLegacyPinThenVerify(t *testing.T) {
	// 路径 B 全程：pair-setup-pin 3 轮 + pair-verify 2 轮
	d := newFakePairDevice(t, "1234")
	_, p := dialPairing(t, d)
	if err := p.PairSetupPin(context.Background(), "1234"); err != nil {
		t.Fatalf("PairSetupPin: %v", err)
	}
	if err := p.PairVerify(context.Background()); err != nil {
		t.Fatalf("PairVerify: %v", err)
	}
	if len(d.requests()) != 5 {
		t.Fatalf("路径 B 共应 5 个请求，实际 %d", len(d.requests()))
	}
}
