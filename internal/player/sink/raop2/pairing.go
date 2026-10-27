package raop2

import (
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"

	"songloft/internal/player/sink/airplay"
)

// Pairing 三路配对状态机（pair-setup / pair-setup-pin / pair-verify），
// 逐段移植 AirPlayClientV2.cs。
//
//	路径 A（HomeKit/transient）：pair-setup 3 轮（TLV，X-Apple-HKP:3/4，
//	  SRP6a-Airplay2 + PS-Msg05 ChaChaPoly + ed25519）→ InitEncryption。
//	路径 B（legacy pin）：pair-setup-pin 3 轮（bplist，SRP6a-Airplay1 +
//	  AES-GCM epk/authTag）→ pair-verify 2 轮（裸 octet-stream，X25519 +
//	  "Pair-Verify-AES-Key"/"IV" + AES-ECB 计数器模式 + ed25519）。
//
// 认证决策树（对齐 Pair()/PairHomekitThenLegacy()）：
// OPTIONS 响应含 Apple-Response → AES 音频路径；401+WWW-Authenticate →
// digest（复用 airplay/digest.go）；设备 features 声明 SupportsTransient
// Pairing → 路径 A（transient），否则路径 B。
type Pairing struct {
	rtsp *RTSPClient

	// 长期 ed25519 身份（pair-setup/pair-verify 签名用）。nil 时 NewPairing
	// 现场生成（transient 语义；持久配对应由调用方存取密钥）。
	privkey ed25519.PrivateKey
	pubkey  []byte

	// homekitPairingID HomeKit 配对 ID（NewHomeKitPairingId 语义：
	// 16 字节，byte0=(b|2)&0xFE，32 位大写 hex）。
	homekitPairingID []byte
	// clientDeviceID SRP 的 client_id / pair-setup-pin 的 user，
	// 逐字对齐 _client_device_id 的硬编码值。
	clientDeviceID []byte

	// sharedSecret pair-setup 的 SRP 会话密钥（Airplay2 为 64B）。
	sharedSecret []byte
	// sessionKey sharedSecret 前 32B（pair-verify 会覆盖为 X25519 共享密钥）。
	sessionKey []byte
}

// clientDeviceIDDefault 逐字复制自 AirPlayClientV2.cs:6363。
const clientDeviceIDDefault = "I1Y67NO1F5VPGFAV"

// NewPairing 建配对状态机。privkey 为空时现场生成 ed25519 密钥。
func NewPairing(rtsp *RTSPClient, privkey ed25519.PrivateKey) *Pairing {
	p := &Pairing{
		rtsp:             rtsp,
		homekitPairingID: NewHomeKitPairingID(),
		clientDeviceID:   []byte(clientDeviceIDDefault),
	}
	if privkey != nil {
		p.privkey = privkey
		p.pubkey = privkey.Public().(ed25519.PublicKey)
	} else {
		_, key, _ := ed25519.GenerateKey(rand.Reader) //nolint:errcheck // ed25519 生成只在熵源失效时出错
		p.privkey = key
		p.pubkey = key.Public().(ed25519.PublicKey)
	}
	return p
}

// NewHomeKitPairingID NewHomeKitPairingId 语义：16 字节 GUID，
// byte0 = (b|2)&0xFE，ToHexString 大写（32 字符）。
func NewHomeKitPairingID() []byte {
	var b [16]byte
	rand.Read(b[:]) //nolint:errcheck // 同上
	b[0] = (b[0] | 2) & 0xFE
	return []byte(hex.EncodeToString(b[:]))
}

// SharedSecret pair-setup 产物（InitEncryption 用）。
func (p *Pairing) SharedSecret() []byte { return p.sharedSecret }

// SessionKey pair-verify 产物（InitEncryption 用）。
func (p *Pairing) SessionKey() []byte { return p.sessionKey }

// TlvReportsError TLV 带 Error 且值非 0（逐字对齐 TlvReportsError）。
func TlvReportsError(t TLV) (code byte, reports bool) {
	e := t.Get(TLVError)
	if len(e) == 0 {
		return 0, false
	}
	return e[0], e[0] != 0
}

// createDeviceInfo device_x ‖ pairingID ‖ pubkey（逐字对齐 CreateDeviceInfo）。
func (p *Pairing) createDeviceInfo(deviceX, pubkey []byte) []byte {
	out := make([]byte, 0, len(deviceX)+len(p.homekitPairingID)+len(pubkey))
	out = append(out, deviceX...)
	out = append(out, p.homekitPairingID...)
	return append(out, pubkey...)
}

var errUnauthorized = errors.New("raop2: 配对被设备拒绝（Unauthorized）")

func pairHeaders(extra map[string]string) map[string]string {
	h := map[string]string{
		"X-Apple-HKP":         "3",
		"X-Apple-Client-Name": "Roon",
		"Content-Type":        "application/octet-stream",
	}
	for k, v := range extra {
		h[k] = v
	}
	return h
}

// PairSetup 路径 A：pair-setup 3 轮（PS-Msg01/03/05）。
//
// transient=true 时 X-Apple-HKP:4、Flags=[16]、密码用 "3939"（对齐
// AirPlayClientV2.cs:3299-3461）；完成后调用方用 SharedSecret()
// InitEncryption（对齐 _003CPairSetup3_003E 响应处理的 transient 分支）。
func (p *Pairing) PairSetup(ctx context.Context, password string, transient bool) error {
	// --- PS-Msg01 ---
	var msg1 TLV
	msg1.Add(TLVState, []byte{1})
	msg1.Add(TLVMethod, []byte{0})
	hkp := "3"
	if transient {
		hkp = "4"
		msg1.Add(TLVFlags, []byte{16})
	}
	resp, err := p.rtsp.Do(ctx, "POST", "/pair-setup", pairHeaders(map[string]string{"X-Apple-HKP": hkp}), msg1.Encode())
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return fmt.Errorf("raop2: pair-setup #1 返回 %d", resp.Status)
	}
	t1, err := DecodeTLV(resp.Body)
	if err != nil {
		return err
	}
	if _, reports := TlvReportsError(t1); reports {
		return fmt.Errorf("raop2: pair-setup #1 被设备拒绝（Error 0x%02X）: %w", t1.Get(TLVError)[0], errUnauthorized)
	}
	serverBytes := t1.Get(TLVPublicKey)
	salt := t1.Get(TLVSalt)
	if len(serverBytes) == 0 || len(salt) == 0 {
		return errors.New("raop2: pair-setup #1 响应缺少 PublicKey/Salt")
	}

	// --- PS-Msg03：SRP6a（Airplay2：3072 群/g=5/SHA-512）---
	if transient {
		password = "3939"
	}
	srp, err := RandomSRP6aAirplay2(rand.Reader, []byte("Pair-Setup"), []byte(password), serverBytes, salt)
	if err != nil {
		return err
	}
	p.sharedSecret = srp.SessionKey
	p.sessionKey = append([]byte(nil), srp.SessionKey[:32]...)

	var msg3 TLV
	msg3.Add(TLVState, []byte{3})
	msg3.Add(TLVPublicKey, srp.ClientPK)
	msg3.Add(TLVProof, srp.ClientProof)
	resp, err = p.rtsp.Do(ctx, "POST", "/pair-setup", pairHeaders(map[string]string{"X-Apple-HKP": hkp}), msg3.Encode())
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return fmt.Errorf("raop2: pair-setup #2 返回 %d", resp.Status)
	}
	t3, err := DecodeTLV(resp.Body)
	if err != nil {
		return err
	}
	if _, reports := TlvReportsError(t3); reports {
		return fmt.Errorf("raop2: pair-setup #2 被设备拒绝（Error 0x%02X）: %w", t3.Get(TLVError)[0], errUnauthorized)
	}

	// --- PS-Msg05：ed25519 签名 + ChaChaPoly 加密 ---
	deviceX := hkdfSHA512(p.sharedSecret, []byte("Pair-Setup-Controller-Sign-Salt"), []byte("Pair-Setup-Controller-Sign-Info"), 32)
	sig := ed25519.Sign(p.privkey, p.createDeviceInfo(deviceX, p.pubkey))
	var inner TLV
	inner.Add(TLVIdentifier, p.homekitPairingID)
	inner.Add(TLVSignature, sig)
	inner.Add(TLVPublicKey, p.pubkey)
	encKey := hkdfSHA512(p.sharedSecret, []byte("Pair-Setup-Encrypt-Salt"), []byte("Pair-Setup-Encrypt-Info"), 32)
	nonce := psMsg05Nonce()
	ct, err := chachaSeal(encKey, nonce, inner.Encode())
	if err != nil {
		return err
	}
	var msg5 TLV
	msg5.Add(TLVState, []byte{5})
	msg5.Add(TLVEncryptedDataWithTag, ct)
	// PS-Msg05 恒 X-Apple-HKP:3（对齐 AirPlayClientV2.cs:3581）。
	resp, err = p.rtsp.Do(ctx, "POST", "/pair-setup", pairHeaders(nil), msg5.Encode())
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return fmt.Errorf("raop2: pair-setup #3 返回 %d", resp.Status)
	}
	t5, err := DecodeTLV(resp.Body)
	if err != nil {
		return err
	}
	if _, reports := TlvReportsError(t5); reports {
		return fmt.Errorf("raop2: pair-setup #3 被设备拒绝（Error 0x%02X）: %w", t5.Get(TLVError)[0], errUnauthorized)
	}
	if transient {
		// 对齐 _003CPairSetup3_003E 响应处理：transient 完成后启用加密 RTSP。
		p.rtsp.InitEncryption(EncryptionChaChaPoly, p.sharedSecret)
	}
	return nil
}

// PairSetupPin 路径 B 第一段：pair-setup-pin 3 轮（bplist）。
//
//	#1 {method:"pin", user:<clientDeviceID>} → {pk, salt}
//	#2 {pk: ClientPK, proof: ClientProof}（SRP6a-Airplay1：2048 群/SHA-1）
//	#3 {epk, authTag}：AES-GCM(key=SHA512("Pair-Setup-AES-Key"‖SK)[0:16],
//	    iv=SHA512("Pair-Setup-AES-IV"‖SK)[0:16] 末字节 +1 **无进位**（Roon
//	    怪癖：`256 == ++byte` 恒 false，经 C# 编译器 CS0652 警告实测钉死），
//	    明文 = 32B 公钥)
func (p *Pairing) PairSetupPin(ctx context.Context, password string) error {
	// --- #1 ---
	req1, err := airplay.EncodeBplist(map[string]any{
		"method": "pin",
		"user":   string(p.clientDeviceID),
	})
	if err != nil {
		return err
	}
	resp, err := p.rtsp.Do(ctx, "POST", "/pair-setup-pin", map[string]string{"Content-Type": "application/x-apple-binary-plist"}, req1)
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return fmt.Errorf("raop2: pair-setup-pin #1 返回 %d", resp.Status)
	}
	challenge, err := airplay.DecodeBplist(resp.Body)
	if err != nil {
		return fmt.Errorf("raop2: pair-setup-pin #1: %w", err)
	}
	pk, salt := challenge["pk"], challenge["salt"]
	if len(pk) == 0 || len(salt) == 0 {
		return errors.New("raop2: pair-setup-pin #1 响应缺少 pk/salt")
	}

	// --- #2：SRP6a（Airplay1：2048 群/g=2/SHA-1）---
	srp, err := RandomSRP6aAirplay1(rand.Reader, p.clientDeviceID, []byte(password), pk, salt)
	if err != nil {
		return err
	}
	req2, err := airplay.EncodeBplist(map[string]any{"pk": srp.ClientPK, "proof": srp.ClientProof})
	if err != nil {
		return err
	}
	resp, err = p.rtsp.Do(ctx, "POST", "/pair-setup-pin", map[string]string{"Content-Type": "application/x-apple-binary-plist"}, req2)
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return fmt.Errorf("raop2: pair-setup-pin #2 返回 %d", resp.Status)
	}
	if _, err := airplay.DecodeBplist(resp.Body); err != nil {
		return fmt.Errorf("raop2: pair-setup-pin #2: %w", err)
	}

	// --- #3：AES-GCM(epk, authTag) ---
	key := sha512Sum(append([]byte("Pair-Setup-AES-Key"), srp.SessionKey...))
	ivRaw := sha512Sum(append([]byte("Pair-Setup-AES-IV"), srp.SessionKey...))
	key, iv := key[:16], ivRaw[:16]
	iv[15]++ // 末字节 +1 带 wrap、无进位（Roon 怪癖，逐字复刻）
	ct, err := gcmSeal(key, iv, p.pubkey)
	if err != nil {
		return err
	}
	req3, err := airplay.EncodeBplist(map[string]any{"epk": ct[:32], "authTag": ct[32:]})
	if err != nil {
		return err
	}
	resp, err = p.rtsp.Do(ctx, "POST", "/pair-setup-pin", map[string]string{"Content-Type": "application/x-apple-binary-plist"}, req3)
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return fmt.Errorf("raop2: pair-setup-pin #3 返回 %d", resp.Status)
	}
	return nil
}

// PairVerify 路径 B 第二段：pair-verify 2 轮（裸 octet-stream，
// 逐字对齐 _003CPairVerify_003Ed__141 / DisplayClass141）。
//
//	#1 body = [00 00 00 01][eph_pub 32][client_pub 32]
//	  响应 body = [device_pub 32][device_sig 64]
//	  shared = X25519(eph_priv, device_pub)
//	  key = SHA512("Pair-Verify-AES-Key"‖shared)[0:16]
//	  iv  = SHA512("Pair-Verify-AES-IV"‖shared)[0:16]
//	  mySig = ed25519.Sign(eph_pub ‖ device_pub, privkey)
//	#2 body = [4B 0][AES-ECB-CTR(key, iv)(device_sig ‖ mySig)[64:128]]
//	  （计数器模式：keystream 块 = ECB(iv)，iv 每块**大端进位**递增）
func (p *Pairing) PairVerify(ctx context.Context) error {
	ephPriv, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	ephPub := ephPriv.PublicKey().Bytes()

	// --- #1 ---
	msg1 := make([]byte, 4+32+len(p.pubkey))
	msg1[0] = 1
	copy(msg1[4:], ephPub)
	copy(msg1[4+len(ephPub):], p.pubkey)
	resp, err := p.rtsp.Do(ctx, "POST", "/pair-verify", map[string]string{"Content-Type": "application/octet-stream"}, msg1)
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return fmt.Errorf("raop2: pair-verify #1 返回 %d", resp.Status)
	}
	if len(resp.Body) < 32+64 {
		return fmt.Errorf("raop2: pair-verify #1 响应过短（%d 字节，需要 96）", len(resp.Body))
	}
	devicePub, deviceSig := resp.Body[:32], resp.Body[32:96]

	deviceKey, err := ecdh.X25519().NewPublicKey(devicePub)
	if err != nil {
		return err
	}
	shared, err := ephPriv.ECDH(deviceKey)
	if err != nil {
		return err
	}
	p.sessionKey = append([]byte(nil), shared...)

	key := sha512Sum(append([]byte("Pair-Verify-AES-Key"), shared...))[:16]
	iv := sha512Sum(append([]byte("Pair-Verify-AES-IV"), shared...))[:16]

	// mySig = Sign(eph_pub ‖ device_pub, privkey)
	msg := append(append([]byte{}, ephPub...), devicePub...)
	mySig := ed25519.Sign(p.privkey, msg)

	// AES-ECB 计数器模式加密 device_sig ‖ mySig，取后 64 字节
	plain := append(append([]byte{}, deviceSig...), mySig...)
	ct := aesECBCounterXOR(key, iv, plain)
	msg2 := make([]byte, 4+64)
	copy(msg2[4:], ct[64:128])

	resp, err = p.rtsp.Do(ctx, "POST", "/pair-verify", map[string]string{"Content-Type": "application/octet-stream"}, msg2)
	if err != nil {
		return err
	}
	if resp.Status != 200 {
		return fmt.Errorf("raop2: pair-verify #2 返回 %d", resp.Status)
	}
	return nil
}

// --- 加密小工具 ---

func sha512Sum(b []byte) []byte {
	s := sha512.Sum512(b)
	return s[:]
}

// psMsg05Nonce [4B 0]["PS-Msg05"]。
func psMsg05Nonce() []byte {
	n := make([]byte, 12)
	copy(n[4:], []byte("PS-Msg05"))
	return n
}

func chachaSeal(key, nonce, plaintext []byte) ([]byte, error) {
	aead, err := chachaNew(key)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, nonce, plaintext, nil), nil
}
