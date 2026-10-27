package raop2

import (
	"crypto/aes"
	"crypto/cipher"

	"golang.org/x/crypto/chacha20poly1305"
)

// chachaNew ChaCha20-Poly1305 AEAD。
func chachaNew(key []byte) (cipher.AEAD, error) {
	return chacha20poly1305.New(key)
}

// newGCM16 16 字节 IV 的 AES-GCM（对齐 C# GcmBlockCipher 的
// AeadParameters(key, 128, iv)：iv 为 16 字节，按 NIST SP 800-38D
// 非 96 位 IV 时 J0 = GHASH(IV‖0‖len)。Go 的 cipher.NewGCM 只支持
// 12 字节，须用 NewGCMWithNonceSize(16)（与 BouncyCastle 同规范）。
func newGCM16(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithNonceSize(block, 16)
}

// chachaSealAAD ChaCha20-Poly1305 加密（带 AAD），返回 ct ‖ tag。
func chachaSealAAD(key, nonce, plaintext, aad []byte) ([]byte, error) {
	aead, err := chachaNew(key)
	if err != nil {
		return nil, err
	}
	return aead.Seal(nil, nonce, plaintext, aad), nil
}

// gcmSeal AES-GCM（128 位 tag，16 字节 IV），返回 ct ‖ tag。
func gcmSeal(key, iv, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM16(key)
	if err != nil {
		return nil, err
	}
	return gcm.Seal(nil, iv, plaintext, nil), nil
}

// gcmOpen AES-GCM 解密（输入 ct ‖ tag，16 字节 IV）。
func gcmOpen(key, iv, ctAndTag []byte) ([]byte, error) {
	gcm, err := newGCM16(key)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, iv, ctAndTag, nil)
}

// aesECBCounterXOR AES-ECB 计数器模式（逐字对齐 AirPlayClientV2.cs:947-961）：
//
//	keystream 块 = ECB(iv)；每块后 iv 按 16 字节**大端进位**递增
//	（num=1; for i=15..0: num += iv[i]; iv[i] = byte(num); num >>= 8）；
//	输出 = plain ⊕ keystream。
func aesECBCounterXOR(key, iv, plain []byte) []byte {
	block, err := aes.NewCipher(key)
	if err != nil {
		panic("raop2: aes: " + err.Error())
	}
	out := make([]byte, len(plain))
	ctr := append([]byte(nil), iv...)
	var ks [16]byte
	for i := 0; i < len(plain); i += 16 {
		block.Encrypt(ks[:], ctr)
		n := 16
		if rem := len(plain) - i; rem < n {
			n = rem
		}
		for j := 0; j < n; j++ {
			out[i+j] = plain[i+j] ^ ks[j]
		}
		// 大端进位递增
		var carry uint32 = 1
		for j := 15; j >= 0; j-- {
			carry += uint32(ctr[j])
			ctr[j] = byte(carry)
			carry >>= 8
		}
	}
	return out
}
