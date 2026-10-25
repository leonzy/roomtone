package raop

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
)

// AESKeySize RAOP 固定 AES-128。
const AESKeySize = 16

// EncryptAESKey 用设备的 RSA 公钥封装 AES key（RSA-OAEP + SHA1）。
//
// RAOP 的 SDP 里 a=rsaaeskey 就是这段密文的 base64。SHA1 是协议规定的，
// 不是安全选择；设备端只认这个填充方式。
func EncryptAESKey(pub *rsa.PublicKey, key []byte) (string, error) {
	if pub == nil {
		return "", errors.New("raop: 缺少设备公钥")
	}
	if len(key) != AESKeySize {
		return "", fmt.Errorf("raop: AES key 长度必须是 %d，实际 %d", AESKeySize, len(key))
	}
	enc, err := rsa.EncryptOAEP(sha1.New(), rand.Reader, pub, key, nil)
	if err != nil {
		return "", fmt.Errorf("raop: RSA 封装失败: %w", err)
	}
	return base64.StdEncoding.EncodeToString(enc), nil
}

// ParsePublicKey 解析设备 TXT 记录里的 pk（PEM 编码公钥）。
func ParsePublicKey(pk string) (*rsa.PublicKey, error) {
	block, _ := pem.Decode([]byte(pk))
	if block == nil {
		return nil, errors.New("raop: 公钥不是 PEM 格式")
	}
	if k, err := x509.ParsePKIXPublicKey(block.Bytes); err == nil {
		if pub, ok := k.(*rsa.PublicKey); ok {
			return pub, nil
		}
		return nil, errors.New("raop: 公钥不是 RSA")
	}
	pub, err := x509.ParsePKCS1PublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("raop: 解析公钥失败: %w", err)
	}
	return pub, nil
}

// NewAESKey 生成会话 AES key 与 IV。
func NewAESKey() (key, iv []byte, err error) {
	key = make([]byte, AESKeySize)
	iv = make([]byte, aes.BlockSize)
	if _, err = rand.Read(key); err != nil {
		return nil, nil, err
	}
	if _, err = rand.Read(iv); err != nil {
		return nil, nil, err
	}
	return key, iv, nil
}

// EncryptFrames 用 AES-128-CBC 加密一整帧 PCM。
//
// RAOP 每帧独立 CBC：IV 固定，长度必须是 16 的整数倍（20ms 立体声 16bit
// 恰好 3528 字节，可整除）。不足一帧的尾巴补零，调用方按原长度发。
func EncryptFrames(key, iv, plain []byte) ([]byte, error) {
	if len(key) != AESKeySize {
		return nil, fmt.Errorf("raop: AES key 长度必须是 %d", AESKeySize)
	}
	if len(iv) != aes.BlockSize {
		return nil, fmt.Errorf("raop: IV 长度必须是 %d", aes.BlockSize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	padded := plain
	if r := len(plain) % aes.BlockSize; r != 0 {
		padded = make([]byte, len(plain)+aes.BlockSize-r)
		copy(padded, plain)
	}
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out, padded)
	return out, nil
}

// DecryptFrames EncryptFrames 的逆运算，测试与排障用。
func DecryptFrames(key, iv, cipherText []byte) ([]byte, error) {
	if len(cipherText)%aes.BlockSize != 0 {
		return nil, errors.New("raop: 密文长度不是块大小的整数倍")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(cipherText))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out, cipherText)
	return out, nil
}
