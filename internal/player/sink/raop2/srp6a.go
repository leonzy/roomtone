package raop2

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha512"
	"errors"
	"hash"
	"io"
	"math/big"
)

// SRP6a 客户端会话，逐字移植 Roon 的 SRP6ClientSession.cs（math/big 实现）。
//
// 两条路径：
//   - Airplay1：2048 位 RFC5054 群（N 常量逐字复制自 SRP6ClientSession.cs）、
//     g=2、SHA-1；SessionKey = SHA1(S‖00000000) ‖ SHA1(S‖00000001)（40B）。
//   - Airplay2：3072 位 RFC3526 群（group 15）、g=5、SHA-512；SessionKey = SHA512(S)（64B）。
//
// 两者共享：k = PaddedPair(N, g, N)、u = PaddedPair(A, B, N)、
// x = H(salt ‖ H(clientID ‖ ':' ‖ pin))、S = (B − k·g^x)^(u·x + a) mod N、
// ClientProof = H(H(N)⊕H(g) ‖ H(clientID) ‖ salt ‖ ClientPK ‖ B ‖ SessionKey)。

var (
	// srpN1 2048 位 RFC5054 群，逐字复制自 SRP6ClientSession.cs 的 _N。
	srpN1 = mustBigFromHex("AC6BDB41324A9A9BF166DE5E1389582FAF72B6651987EE07FC3192943DB56050A37329CBB4A099ED8193E0757767A13DD52312AB4B03310DCD7F48A9DA04FD50E8083969EDB767B0CF6095179A163AB3661A05FBD5FAAAE82918A9962F0B93B855F97993EC975EEAA80D740ADBF4FF747359D041D5C33EA71D281E446B14773BCA97B43A23FB801676BD207A436C6481F1D2B9078717461A5B9D32E688F87748544523B524B0D57D5EA77A2775D2ECFA032CFBDBF52FB3786160279004E57AE6AF874E7303CE53299CCC041C7BC308D82A5698F3A8D0C38271AE35F8E9DBFBB694B5C803D89F7AE435DE236D525F54759B65E372FCD68EF20FA7111F9E4AFF73")
	srpG1 = big.NewInt(2)

	// srpN2 3072 位 RFC3526 群（group 15），逐字复制自 SRP6ClientSession.cs 的 InitAirplay2。
	srpN2 = mustBigFromHex("FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7EDEE386BFB5A899FA5AE9F24117C4B1FE649286651ECE45B3DC2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F83655D23DCA3AD961C62F356208552BB9ED529077096966D670C354E4ABC9804F1746C08CA18217C32905E462E36CE3BE39E772C180E86039B2783A2EC07A28FB5C55DF06F4C52C9DE2BCBF6955817183995497CEA956AE515D2261898FA051015728E5A8AAAC42DAD33170D04507A33A85521ABDF1CBA64ECFB850458DBEF0A8AEA71575D060C7DB3970F85A6E1E4C7ABF5AE8CDB0933D71E8C94E04A25619DCEE3D2261AD2EE6BF12FFA06D98A0864D87602733EC86A64521F2B18177B200CBBE117577A615D6C770988C0BAD946E208E24FA074E5AB3143DB5BFCE0FD108E4B82D120A93AD2CAFFFFFFFFFFFFFFFF")
	srpG2 = big.NewInt(5)
)

func mustBigFromHex(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 16)
	if !ok {
		panic("srp6a: 非法 16 进制常量")
	}
	return n
}

// SRP6a 一次会话的计算结果。
type SRP6a struct {
	SessionKey  []byte
	ClientPK    []byte
	ClientProof []byte
}

// SHA1PaddedPair SHA1(pad(n1) ‖ pad(n2))，pad 到 N 的字节长（左补零对齐）。
func SHA1PaddedPair(n1, n2, N *big.Int) []byte {
	L := (N.BitLen() + 7) / 8
	sum := sha1.Sum(append(paddedBytes(n1, L), paddedBytes(n2, L)...))
	return sum[:]
}

// SHA512PaddedPair SHA512 版本。
func SHA512PaddedPair(n1, n2, N *big.Int) []byte {
	L := (N.BitLen() + 7) / 8
	sum := sha512.Sum512(append(paddedBytes(n1, L), paddedBytes(n2, L)...))
	return sum[:]
}

// paddedBytes GetPadded：unsigned 大端，短于 length 左补零。
func paddedBytes(n *big.Int, length int) []byte {
	raw := n.Bytes() // big-endian unsigned
	if len(raw) < length {
		out := make([]byte, length)
		copy(out[length-len(raw):], raw)
		return out
	}
	return raw
}

// NewSRP6aAirplay1 InitAirplay1（2048 群/g=2/SHA-1），a 显式注入（测试确定性）。
func NewSRP6aAirplay1(a *big.Int, clientID, pin, serverBytes, salt []byte) *SRP6a {
	N, g := srpN1, srpG1
	A := new(big.Int).Exp(g, a, N)
	k := new(big.Int).SetBytes(SHA1PaddedPair(N, g, N))
	B := new(big.Int).SetBytes(serverBytes)
	u := new(big.Int).SetBytes(SHA1PaddedPair(A, B, N))

	// x = SHA1(salt ‖ SHA1(clientID ‖ ':' ‖ pin))
	inner := sha1.Sum(append(append(append([]byte{}, clientID...), 0x3a), pin...))
	xHash := sha1.Sum(append(append([]byte{}, salt...), inner[:]...))
	x := new(big.Int).SetBytes(xHash[:])

	// S = (B − k·g^x)^(u·x + a) mod N
	S := srpComputeS(N, g, k, a, A, B, u, x)

	// SessionKey = SHA1(S‖00000000) ‖ SHA1(S‖00000001)
	sk1 := sha1.Sum(append(append([]byte{}, S.Bytes()...), 0, 0, 0, 0))
	sk2 := sha1.Sum(append(append([]byte{}, S.Bytes()...), 0, 0, 0, 1))
	sessionKey := append(sk1[:], sk2[:]...)

	return &SRP6a{
		SessionKey:  sessionKey,
		ClientPK:    A.Bytes(),
		ClientProof: srpProof(sha1.New, N, g, clientID, serverBytes, salt, A.Bytes(), sessionKey),
	}
}

// NewSRP6aAirplay2 InitAirplay2（3072 群/g=5/SHA-512），a 显式注入。
func NewSRP6aAirplay2(a *big.Int, clientID, pin, serverBytes, salt []byte) *SRP6a {
	N, g := srpN2, srpG2
	A := new(big.Int).Exp(g, a, N)
	k := new(big.Int).SetBytes(SHA512PaddedPair(N, g, N))
	B := new(big.Int).SetBytes(serverBytes)
	u := new(big.Int).SetBytes(SHA512PaddedPair(A, B, N))

	inner := sha512.Sum512(append(append(append([]byte{}, clientID...), 0x3a), pin...))
	xHash := sha512.Sum512(append(append([]byte{}, salt...), inner[:]...))
	x := new(big.Int).SetBytes(xHash[:])

	S := srpComputeS(N, g, k, a, A, B, u, x)
	sk := sha512.Sum512(S.Bytes())
	sessionKey := sk[:]

	return &SRP6a{
		SessionKey:  sessionKey,
		ClientPK:    A.Bytes(),
		ClientProof: srpProof(sha512.New, N, g, clientID, serverBytes, salt, A.Bytes(), sessionKey),
	}
}

// srpComputeS S = (B − k·g^x)^(u·x + a) mod N。
//
// C# 的 BigInteger 减法可为负、ModPow 按模意义处理；Go 的 big.Exp 要求
// 底非负，因此先 Mod 到 [0, N)。
func srpComputeS(N, g, k, a, A, B, u, x *big.Int) *big.Int {
	gx := new(big.Int).Exp(g, x, N)
	base := new(big.Int).Sub(B, new(big.Int).Mul(gx, k))
	base.Mod(base, N)
	exp := new(big.Int).Add(new(big.Int).Mul(u, x), a)
	return new(big.Int).Exp(base, exp, N)
}

// srpProof ClientProof = H(hXOR ‖ H(clientID) ‖ salt ‖ ClientPK ‖ B ‖ SessionKey)，
// 其中 hXOR = H(N unsigned) ⊕ H(g unsigned)。
func srpProof(newHash func() hash.Hash, N, g *big.Int, clientID, serverBytes, salt, clientPK, sessionKey []byte) []byte {
	hN := newHash()
	hN.Write(N.Bytes())
	hNsum := hN.Sum(nil)
	hG := newHash()
	hG.Write(g.Bytes())
	hGsum := hG.Sum(nil)
	hXOR := make([]byte, len(hNsum))
	for i := range hNsum {
		hXOR[i] = hNsum[i] ^ hGsum[i]
	}

	hID := newHash()
	hID.Write(clientID)

	p := newHash()
	p.Write(hXOR)
	p.Write(hID.Sum(nil))
	p.Write(salt)
	p.Write(clientPK)
	p.Write(serverBytes)
	p.Write(sessionKey)
	return p.Sum(nil)
}

// RandomSRP6aAirplay1 生产路径：a ∈ [2^255, N−1]（CreateRandomInRange 语义，
// 对齐 InitAirplay1 的 min = 1<<(min(256, N.BitLength/2)−1)）。
func RandomSRP6aAirplay1(rng io.Reader, clientID, pin, serverBytes, salt []byte) (*SRP6a, error) {
	if rng == nil {
		rng = rand.Reader
	}
	a, err := randomInRange(rng, new(big.Int).Lsh(big.NewInt(1), 255), new(big.Int).Sub(srpN1, big.NewInt(1)))
	if err != nil {
		return nil, err
	}
	return NewSRP6aAirplay1(a, clientID, pin, serverBytes, salt), nil
}

// RandomSRP6aAirplay2 生产路径：a ∈ [2^255, 2^256−1]
// （CreateRandomWithBitLength(256) 语义）。
func RandomSRP6aAirplay2(rng io.Reader, clientID, pin, serverBytes, salt []byte) (*SRP6a, error) {
	if rng == nil {
		rng = rand.Reader
	}
	upper := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	a, err := randomInRange(rng, new(big.Int).Lsh(big.NewInt(1), 255), upper)
	if err != nil {
		return nil, err
	}
	return NewSRP6aAirplay2(a, clientID, pin, serverBytes, salt), nil
}

// randomInRange CreateRandomInRange 语义：读 (max.BitLength+7)/8 字节，
// 落在 [min, max] 才接受，否则重读。
func randomInRange(rng io.Reader, min, max *big.Int) (*big.Int, error) {
	if min.Sign() < 0 || max.Sign() <= 0 || min.Cmp(max) > 0 {
		return nil, errors.New("srp6a: 非法的随机区间")
	}
	buf := make([]byte, (max.BitLen()+7)/8)
	for {
		if _, err := io.ReadFull(rng, buf); err != nil {
			return nil, err
		}
		v := new(big.Int).SetBytes(buf)
		if v.Cmp(min) >= 0 && v.Cmp(max) <= 0 {
			return v, nil
		}
	}
}
