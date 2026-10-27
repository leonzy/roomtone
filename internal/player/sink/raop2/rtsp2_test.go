package raop2

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"
)

// AC-P1①：HKDF 派生——Control 通道写密钥 info="Control-Write-Encryption-Key"；
// Events 通道写密钥 info="Control-Read-Encryption-Key"（**交叉**）。
func TestEncryptionContextKeyDerivation(t *testing.T) {
	secret := bytes.Repeat([]byte{0x42}, 64)

	ctl := NewEncryptionContext(EncryptionChaChaPoly, ChannelControl, secret)
	// 用 hkdfSHA512 重算期望值（同公式独立验证）
	wantW := hkdfSHA512(secret, []byte("Control-Salt"), []byte("Control-Write-Encryption-Key"), 32)
	wantR := hkdfSHA512(secret, []byte("Control-Salt"), []byte("Control-Read-Encryption-Key"), 32)
	if !bytes.Equal(ctl.encryptionKey, wantW) {
		t.Fatal("Control 写密钥派生不符")
	}
	if !bytes.Equal(ctl.decryptionKey, wantR) {
		t.Fatal("Control 读密钥派生不符")
	}

	evt := NewEncryptionContext(EncryptionChaChaPoly, ChannelEvents, secret)
	// Events 交叉：写 = "Control-Read-Encryption-Key"、读 = "Events-Write-Encryption-Key"
	wantEW := hkdfSHA512(secret, []byte("Events-Salt"), []byte("Control-Read-Encryption-Key"), 32)
	wantER := hkdfSHA512(secret, []byte("Events-Salt"), []byte("Events-Write-Encryption-Key"), 32)
	if !bytes.Equal(evt.encryptionKey, wantEW) {
		t.Fatal("Events 写密钥应交叉派生（Control-Read-Encryption-Key）")
	}
	if !bytes.Equal(evt.decryptionKey, wantER) {
		t.Fatal("Events 读密钥应为 Events-Write-Encryption-Key")
	}
}

// AC-P1②：帧格式——2048B 明文 → 2 块 [2B LE 1024][1024B ct][16B tag]；
// AAD = 各块 2B 长度前缀；nonce = [4B 0][8B LE counter]（逐块递增）。
func TestEncryptionContextFrameFormat(t *testing.T) {
	secret := bytes.Repeat([]byte{0x07}, 64)
	plaintext := make([]byte, 2048)
	for i := range plaintext {
		plaintext[i] = byte(i % 253)
	}

	// 用设备侧（独立重算）验证帧结构
	enc := NewEncryptionContext(EncryptionChaChaPoly, ChannelControl, secret)
	out := enc.Encrypt(plaintext)
	if len(out) != 2048+2*(16+2) {
		t.Fatalf("总长 = %d，期望 2068", len(out))
	}
	// 块 1：[2B LE 1024]
	if n := binary.LittleEndian.Uint16(out[0:2]); n != 1024 {
		t.Fatalf("块 1 长度前缀 = %d，期望 1024", n)
	}
	// 块 2：位于 2+1024+16 = 1042
	if n := binary.LittleEndian.Uint16(out[1042:1044]); n != 1024 {
		t.Fatalf("块 2 长度前缀 = %d，期望 1024", n)
	}

	// 独立解密：密钥重派生 + 手工分块解（不经过 Decrypt 的对称路径）
	aead, _ := chachaNew(hkdfSHA512(secret, []byte("Control-Salt"), []byte("Control-Write-Encryption-Key"), 32))
	nonce := make([]byte, 12)
	var pt []byte
	off := 0
	for counter := uint64(0); off < len(out); counter++ {
		n := int(binary.LittleEndian.Uint16(out[off : off+2]))
		binary.LittleEndian.PutUint64(nonce[4:12], counter)
		block, err := aead.Open(nil, nonce, out[off+2:off+2+n+16], out[off:off+2])
		if err != nil {
			t.Fatalf("第 %d 块独立解密失败: %v", counter, err)
		}
		pt = append(pt, block...)
		off += 2 + n + 16
	}
	if !bytes.Equal(pt, plaintext) {
		t.Fatal("独立解密结果与明文不符")
	}

	// AAD 必须是长度前缀：篡改长度前缀应解密失败
	tampered := append([]byte(nil), out...)
	tampered[0] ^= 1 // 长度前缀 1024 → 1025（低字节异或 1）
	if _, err := aead.Open(nil, nonce, tampered[2:2+1025+16], tampered[0:2]); err == nil {
		t.Fatal("AAD 校验应失败（长度前缀被篡改）")
	}
}

// peerContext 设备侧上下文：密钥互换（同方向同 info 串——客户端用
// "Control-Write" 加密请求，设备也用 "Control-Write" 派生的密钥解密请求，
// 即设备的读密钥 = 客户端的写密钥；Roon 客户端与真机互通即证明此约定）。
func peerContext(ctx *EncryptionContext) *EncryptionContext {
	return &EncryptionContext{
		encType:           ctx.encType,
		encryptionKey:     ctx.decryptionKey,
		decryptionKey:     ctx.encryptionKey,
		encryptionCounter: 0,
		decryptionCounter: 0,
		encryptor:         mustChaCha(ctx.decryptionKey),
		decryptor:         mustChaCha(ctx.encryptionKey),
	}
}

// AC-P1③：解密流式——密文分两次到达（残留缓冲）仍正确解出；
// 加解密 counter 独立（各自从 0 递增）。
func TestEncryptionContextRoundTrip(t *testing.T) {
	secret := bytes.Repeat([]byte{0x99}, 64)
	enc := NewEncryptionContext(EncryptionChaChaPoly, ChannelControl, secret)
	// 设备侧：读密钥 = 客户端写密钥（同方向同 info 串）
	dec := peerContext(enc)

	// 多个报文连续加解密（counter 持续递增）
	msgs := [][]byte{
		bytes.Repeat([]byte{0xAA}, 100),
		bytes.Repeat([]byte{0xBB}, 1024), // 恰一块
		bytes.Repeat([]byte{0xCC}, 1025), // 跨两块
		bytes.Repeat([]byte{0xDD}, 0),    // 空（0 块 → 1 空块）
	}
	for i, msg := range msgs {
		ct := enc.Encrypt(msg)
		pt, consumed, err := dec.Decrypt(ct)
		if err != nil {
			t.Fatalf("第 %d 条解密失败: %v", i, err)
		}
		if !bytes.Equal(pt, msg) {
			t.Fatalf("第 %d 条往返不符（%d vs %d 字节）", i, len(pt), len(msg))
		}
		if consumed != len(ct) {
			t.Fatalf("第 %d 条 consumed = %d，期望 %d", i, consumed, len(ct))
		}
	}

	// 分两次到达：前半段解到块边界（consumed），剩余与前半未消费部分
	// 拼接后再解（模拟 TCP 分段的残留缓冲机制）
	msg := bytes.Repeat([]byte{0xEE}, 2000)
	ct := enc.Encrypt(msg)
	half := len(ct) / 2 // 1018：不足第一块（1042），consumed 应为 0
	pt1, c1, err := dec.Decrypt(ct[:half])
	if err != nil {
		t.Fatalf("前半解密失败: %v", err)
	}
	if c1 != 0 || len(pt1) != 0 {
		t.Fatalf("不足一块时应停在块边界：consumed=%d pt=%d", c1, len(pt1))
	}
	pt2, c2, err := dec.Decrypt(ct[c1:])
	if err != nil {
		t.Fatalf("整段解密失败: %v", err)
	}
	if c2 != len(ct) || !bytes.Equal(pt2, msg) {
		t.Fatalf("拼接解密不符：consumed=%d pt=%d", c2, len(pt2))
	}

	// 分段点落在块中间：前半含完整第一块 + 第二块碎片
	// （沿用锁步中的 dec——加密端与解密端的 counter 必须从会话起点同步推进）
	ct = enc.Encrypt(msg)
	split := 2 + 1024 + 16 + 100 // 第一块完整 + 100 字节碎片
	pt1, c1, err = dec.Decrypt(ct[:split])
	if err != nil {
		t.Fatalf("前半（含整块）解密失败: %v", err)
	}
	if c1 != 2+1024+16 || len(pt1) != 1024 {
		t.Fatalf("应恰好消费第一块：consumed=%d pt=%d", c1, len(pt1))
	}
	pt2, _, err = dec.Decrypt(ct[c1:])
	if err != nil {
		t.Fatalf("剩余解密失败: %v", err)
	}
	if !bytes.Equal(append(pt1, pt2...), msg) {
		t.Fatal("分段到达的解密拼接不符")
	}
}

// AC-P1④：未启用加密时行为与 V1 Client 一致（明文直通）。
func TestRTSPClientPlaintextGolden(t *testing.T) {
	req := BuildRequest("OPTIONS", "*", 1, "", map[string]string{"Content-Type": "application/octet-stream"}, []byte{1, 2, 3})
	want := "OPTIONS * RTSP/1.0\r\n" +
		"Content-Type: application/octet-stream\r\n" +
		"CSeq: 1\r\n" +
		"Content-Length: 3\r\n" +
		"\r\n" +
		"\x01\x02\x03"
	if string(req) != want {
		t.Fatalf("明文请求不符:\n got=%q\nwant=%q", req, want)
	}
}

// 加密 RTSP 端到端：InitEncryption 后请求体密文形态、响应体自动解密。
func TestRTSPClientEncryptedRoundTrip(t *testing.T) {
	d := newFakePairDevice(t, "3939")
	// 设备侧镜像：用同一 secret 建反向上下文（设备读 = 客户端写）
	secret := bytes.Repeat([]byte{0x5A}, 64)
	// 设备侧 = peer 上下文（读密钥 = 客户端写密钥，同方向同 info 串）
	cliCtx := NewEncryptionContext(EncryptionChaChaPoly, ChannelControl, secret)
	devDec := peerContext(cliCtx)
	devEnc := peerContext(cliCtx)

	cl, err := Dial(context.Background(), d.addr(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()

	// 阶段一：未加密——明文请求走原路由（确认明文路径不受影响）
	var msg1 TLV
	msg1.Add(TLVState, []byte{1})
	if _, err := cl.Do(context.Background(), "POST", "/pair-setup", map[string]string{"Content-Type": "application/octet-stream"}, msg1.Encode()); err != nil {
		t.Fatalf("明文请求: %v", err)
	}

	// 阶段二：启用加密（此后所有请求密文）。设备侧换上解密路由：
	// 请求体先解密再按明文处理，响应体加密。
	d.mu.Lock()
	origRoute := d.route
	d.route = func(method, path string, body []byte, failAt string) (int, string, []byte) {
		pt, _, err := devDec.Decrypt(body)
		if err != nil {
			t.Errorf("设备侧请求体解密失败: %v", err)
			pt = body
		}
		status, ctype, respBody := origRoute(method, path, pt, failAt)
		return status, ctype, devEnc.Encrypt(respBody)
	}
	d.mu.Unlock()
	cl.InitEncryption(EncryptionChaChaPoly, secret)
	if !cl.EncryptionEnabled() {
		t.Fatal("InitEncryption 后应报告已启用")
	}
	// 加密后的请求走 PS-Msg01（State=1 → 2，设备侧无需验 proof——
	// 加密往返测试只验证密文层，SRP 互验在 TestPairSetupThreeRounds）
	var msg3 TLV
	msg3.Add(TLVState, []byte{1})
	resp, err := cl.Do(context.Background(), "POST", "/pair-setup", pairHeaders(nil), msg3.Encode())
	if err != nil {
		t.Fatalf("加密请求: %v", err)
	}
	if resp.Status != 200 {
		t.Fatalf("加密请求状态 = %d", resp.Status)
	}
	// 响应体应已被自动解密为明文 TLV（State=4）
	tl, err := DecodeTLV(resp.Body)
	if err != nil {
		t.Fatalf("响应体不是明文 TLV（未自动解密？）: %v", err)
	}
	if got := tl.Get(TLVState); len(got) != 1 || got[0] != 2 {
		t.Fatalf("解密后的响应 State = %v，期望 [2]", got)
	}
}
