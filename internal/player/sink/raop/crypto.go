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
	"math/big"
)

// AESKeySize RAOP 固定 AES-128。
const AESKeySize = 16

// Legacy RAOP RSA receivers share the AirPort public key. TXT pk belongs to
// newer pairing protocols and is not required for et=1 RSA key exchange.
// Public modulus used by RAOP-Player and other interoperable RAOP senders.
func LegacyPublicKeyPEM() string {
	const modulus = "59dE8qLieItsH1WgjrcFRKj6eUWqi+bGLOX1HL3U3GhC/j0Qg90u3sG/1CUtwC5vOYvfDmFI6oSFXi5ELabWJmT2dKHzBJKa3k9ok+8t9ucRqMd6DZHJ2YCCLlDRKSKv6kDqnw4UwPdpOMXziC/AMj3Z/lUVX1G7WSHCAWKf1zNS1eLvqr+boEjXuBOitnZ/bDzPHrTOZz0Dew0uowxf/+sG+NCK3eQJVxqcaJ/vEHKIVd2M+5qL71yJQ+87X6oV3eaYvt3zWZYD6z5vYTcrtij2VZ9Zmni/UAaHqn9JdsBWLUEpVviYnhimNVvYFZeCXg/IdTQ+x4IRdiXNv5hEew=="
	n, _ := base64.StdEncoding.DecodeString(modulus)
	key := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: 65537}
	return string(pem.EncodeToMemory(&pem.Block{Type: "RSA PUBLIC KEY", Bytes: x509.MarshalPKCS1PublicKey(key)}))
}

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
	return base64.RawStdEncoding.EncodeToString(enc), nil
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
// RAOP 每包独立 CBC，IV 固定。只加密完整块，尾部保持明文；不得补零改变包长。
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
	out := append([]byte(nil), plain...)
	n := len(plain) / aes.BlockSize * aes.BlockSize
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(out[:n], plain[:n])
	return out, nil
}

// DecryptFrames EncryptFrames 的逆运算，测试与排障用。
func DecryptFrames(key, iv, cipherText []byte) ([]byte, error) {
	if len(iv) != aes.BlockSize {
		return nil, errors.New("raop: IV 长度错误")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := append([]byte(nil), cipherText...)
	n := len(cipherText) / aes.BlockSize * aes.BlockSize
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(out[:n], cipherText[:n])
	return out, nil
}
