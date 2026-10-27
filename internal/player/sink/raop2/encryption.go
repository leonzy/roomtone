package raop2

import (
	"crypto/cipher"
	"crypto/sha512"
	"fmt"
	"io"

	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// EncryptionType 加密类型（AudioPacketEncryptionType.cs）。
type EncryptionType int

const (
	// EncryptionNone 明文。
	EncryptionNone EncryptionType = iota
	// EncryptionAES RSA 协商的 AES（音频包路径，非本上下文）。
	EncryptionAES
	// EncryptionChaChaPoly ChaCha20-Poly1305（加密 RTSP / V2 音频）。
	EncryptionChaChaPoly
)

// ChannelType 通道类型（AirPlayChannelType.cs）——决定 HKDF 的 salt/info。
type ChannelType int

const (
	// ChannelControl 控制通道：写 "Control-Write-Encryption-Key" /
	// 读 "Control-Read-Encryption-Key"，salt "Control-Salt"。
	ChannelControl ChannelType = iota
	// ChannelEvents 事件通道：密钥**交叉**——写 "Control-Read-Encryption-Key"、
	// 读 "Events-Write-Encryption-Key"，salt "Events-Salt"（EncryptionContext.cs
	// 的既定行为，不是笔误）。
	ChannelEvents
)

const (
	encBlockMax = 1024 // 明文分块上限
	encTagLen   = 16   // Poly1305 tag
)

// EncryptionContext 加密 RTSP / Events 通道的加解密上下文，逐字移植
// Roon 的 EncryptionContext.cs（仅 ChaChaPoly）。
//
// 线格式：明文按 1024B 分块，每块输出 [2B LE 块长][密文][16B tag]；
// **AAD = 2 字节长度前缀**；nonce = [4B 0][8B LE counter]（counter 从 0
// 逐块递增，加解密各自独立计数）。
type EncryptionContext struct {
	encType EncryptionType

	encryptionKey []byte
	decryptionKey []byte

	encryptionCounter uint64
	decryptionCounter uint64

	encryptor cipher.AEAD
	decryptor cipher.AEAD
}

// NewEncryptionContext 派生双向密钥并建上下文。
func NewEncryptionContext(encType EncryptionType, channel ChannelType, sharedSecret []byte) *EncryptionContext {
	if encType != EncryptionChaChaPoly {
		panic("encryption: this constructor only supports chacha20poly1305 encryption")
	}
	var saltW, infoW, saltR, infoR []byte
	switch channel {
	case ChannelControl:
		saltW, infoW = []byte("Control-Salt"), []byte("Control-Write-Encryption-Key")
		saltR, infoR = []byte("Control-Salt"), []byte("Control-Read-Encryption-Key")
	case ChannelEvents:
		// 密钥交叉（EncryptionContext.cs 既定行为）。
		saltW, infoW = []byte("Events-Salt"), []byte("Control-Read-Encryption-Key")
		saltR, infoR = []byte("Events-Salt"), []byte("Events-Write-Encryption-Key")
	default:
		panic(fmt.Sprintf("encryption: channel type %d is not yet implemented", channel))
	}
	encKey := hkdfSHA512(sharedSecret, saltW, infoW, 32)
	decKey := hkdfSHA512(sharedSecret, saltR, infoR, 32)
	return &EncryptionContext{
		encType:       encType,
		encryptionKey: encKey,
		decryptionKey: decKey,
		encryptor:     mustChaCha(encKey),
		decryptor:     mustChaCha(decKey),
	}
}

// hkdfSHA512 HKDF（RFC 5869）的 Extract-Expand，对齐 C# 的
// HKDF.DeriveKey(SHA512, secret, length, salt, info)。
func hkdfSHA512(secret, salt, info []byte, length int) []byte {
	reader := hkdf.New(sha512.New, secret, salt, info)
	out := make([]byte, length)
	if _, err := io.ReadFull(reader, out); err != nil {
		panic("encryption: hkdf: " + err.Error())
	}
	return out
}

// Encrypt 加密（分块 + AAD + LE counter，见类型注释）。
func (e *EncryptionContext) Encrypt(plaintext []byte) []byte {
	if e.encType != EncryptionChaChaPoly {
		return plaintext
	}
	aead := e.encryptor
	nonce := make([]byte, 12)
	blocks := 1 + (len(plaintext)-1)/encBlockMax
	out := make([]byte, 0, len(plaintext)+blocks*(encTagLen+2))
	off := 0
	for i := 0; i < blocks; i++ {
		n := len(plaintext) - off
		if n > encBlockMax {
			n = encBlockMax
		}
		// [2B LE 块长]
		lenPrefix := []byte{byte(n), byte(n >> 8)}
		out = append(out, lenPrefix...)
		// nonce = [4B 0][8B LE counter]
		putUint64LE(nonce[4:12], e.encryptionCounter)
		e.encryptionCounter++
		// 密文 + tag，AAD = 长度前缀
		ct := aead.Seal(nil, nonce, plaintext[off:off+n], lenPrefix)
		out = append(out, ct...)
		off += n
	}
	return out
}

// Decrypt 解密（流式分块）。返回明文与已消费的字节数 consumed（对齐
// EncryptionContext.Decrypt 的 `int Decrypt(ct, count, out pt)` 语义）：
// 尾部不足一个完整块时停在块边界，调用方把 ct[consumed:] 与后续数据
// 拼接后再次调用（TCP 分段到达的残留缓冲机制）。
func (e *EncryptionContext) Decrypt(ciphertext []byte) (plaintext []byte, consumed int, err error) {
	if e.encType != EncryptionChaChaPoly {
		return ciphertext, len(ciphertext), nil
	}
	aead := e.decryptor
	nonce := make([]byte, 12)
	var out []byte
	i := 0
	for i < len(ciphertext) && len(ciphertext)-i >= 2 {
		n := int(ciphertext[i]) | int(ciphertext[i+1])<<8 // LE
		if len(ciphertext)-i < 2+n+encTagLen {
			break
		}
		putUint64LE(nonce[4:12], e.decryptionCounter)
		e.decryptionCounter++
		pt, err := aead.Open(nil, nonce, ciphertext[i+2:i+2+n+encTagLen], ciphertext[i:i+2])
		if err != nil {
			return nil, i, fmt.Errorf("encryption: 解密第 %d 块失败: %w", e.decryptionCounter, err)
		}
		out = append(out, pt...)
		i += 2 + n + encTagLen
	}
	return out, i, nil
}

func mustChaCha(key []byte) cipher.AEAD {
	a, err := chacha20poly1305.New(key)
	if err != nil {
		panic("encryption: " + err.Error())
	}
	return a
}

func putUint64LE(b []byte, v uint64) {
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * i))
	}
}
