package raop2

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"math/big"
	"testing"
)

// golden 向量由独立 Python 参考实现（/tmp/srp6a_ref.py，逐行对齐
// SRP6ClientSession.cs 的 InitAirplay1/InitAirplay2）预计算，固定 a 注入。
var (
	srpClientID = []byte("Roomtone-Test")
	srpPin      = []byte("1234")
	srpSalt     = []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}

	// Airplay1：2048 位 RFC5054 群，a = 0x404142..5f（32 字节）
	srpA1, _           = hex.DecodeString("404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f")
	srpB1, _           = hex.DecodeString("101112131415161718191a1b1c1d1e1f202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f606162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9fa0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebfc0c1c2c3c4c5c6c7c8c9cacbcccdcecfd0d1d2d3d4d5d6d7d8d9dadbdcdddedfe0e1e2e3e4e5e6e7e8e9eaebecedeeeff0f1f2f3f4f5f6f7f8f9fafbfcfdfeff000102030405060708090a0b0c0d0e0f")
	srp1ClientPK, _    = hex.DecodeString("937c7b0538da06d03f23cdbc517a103be9e9bcc12d10ff4739a6d2ad4bf4659d15a34d340b4ebfff7004fbc2eed5572552bd03e4fdfc3be08cbf9156eff5663734e1a012a2a155e09259571d4f4541211480155063258c3861a98625b843d775206031fa1aa00d919ea734c984ac067654baca3007677125bcb4b187979306a524c6b231a665dec628a7a03cd4cf4078e794a3d76b9f9134523adbf66393ec4349b38888d198159b7200abdb1383870fced29e72c8a79f04bab0cd39d5dc1bade5ee28b627f22ef6078f72be3b583e799ac199bb6c7fd411debd53ceb120f5c58c14ec4b58d5b26a6fb2c906d4f10ebc1acbd92793a4c86e341cafbf6bfa41f1")
	srp1SessionKey, _  = hex.DecodeString("d36d420996a20d725fbcdec8a989f83d005eebaa271712bbc860afd9137d11e27d4818b4f0e797f1")
	srp1ClientProof, _ = hex.DecodeString("99fe48edadf815c5cd5c774b03df869babad5d9d")

	// Airplay2：3072 位 RFC3526 群（group 15），a = 0x807f7e..61
	srpA2, _           = hex.DecodeString("807f7e7d7c7b7a797877767574737271706f6e6d6c6b6a696867666564636261")
	srpB2, _           = hex.DecodeString("202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f606162636465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f808182838485868788898a8b8c8d8e8f909192939495969798999a9b9c9d9e9fa0a1a2a3a4a5a6a7a8a9aaabacadaeafb0b1b2b3b4b5b6b7b8b9babbbcbdbebfc0c1c2c3c4c5c6c7c8c9cacbcccdcecfd0d1d2d3d4d5d6d7d8d9dadbdcdddedf")
	srp2ClientPK, _    = hex.DecodeString("29fe44610430810de1700037514e255df1f3aa00cdd6034ef99c7e8e741108e9896df2b2939af27b5e5185f300895c18fb64d366a6a9a96a342b101f7c4a18e2c690ef2a33366b686969b7b9082e5d232e8b77e3c41791168ff32c7d1b3365e900ddc044beb3c125cb61357d51a8beebe3ed56e22556a6a5dd314a2eb7f78af320bc7c39327cf4b46ba75a2b2f06c375e253789d432a01b8c09b43f528d26b1b9d0f53c7d54b88ad9299c2ee06a21861e87025265118013441b9d38f74c321dca3f2fbfedecec56201728f9add74e96f13bd01a51e15e002b5689b24d63c432b5bc58f5cb833ea649de475134243fa89b52dd9effde743f2fe47fe7b6667c5e83c936eef8390247b85ba0c4c2c0beda292a4c1084f71ff2bad189870086b97293f1bc44d724b4bdf1651e97d54b90b3c29e065494bdb05cdaa24b842fdcdc51753d9abe9450723490d566c6e085682ea3468f2d1d34fd0f1073a2baf0bcd1a0bda2204bfcb71c923b2d8a2b96050bf94a35bde8febfcfa9501a3127af87fe05c")
	srp2SessionKey, _  = hex.DecodeString("0dc5d4ad419a292f2260027cee9faf015c6ceb631e1095c52977697b56a5ae2db32151bb9fc00eb03d29be45fe745dde78ee4fe73a874c44376a9d21c715aa48")
	srp2ClientProof, _ = hex.DecodeString("17207cffa1b799dcf50af28d7c3b3b1fdc72e04c70c544a7267eb23f5333fe0eddd174386a9710b6a0b84fb73719ab73698f65661438524b91185a8d77b0b24c")
)

func TestSHA1PaddedPair(t *testing.T) {
	// H(pad(N) || pad(g))，pad 到 N 字节长；g=2 → 255 个前导零 + 0x02。
	got := SHA1PaddedPair(srpN1, srpG1, srpN1)
	want := "a56303f32c60e599e82c396f0d57f1b344a7313c"
	if hex.EncodeToString(got) != want {
		t.Fatalf("SHA1PaddedPair = %x，期望 %s", got, want)
	}
	got512 := SHA512PaddedPair(srpN2, srpG2, srpN2)
	want512 := "a9c2e2559bf0ebb53f0cbbf62282906bede7f2182f00678211fbd5bde5b285033a4993503b87397f9be5ec02080fedbc0835587ad039060879b8621e8c3659e0"
	if hex.EncodeToString(got512) != want512 {
		t.Fatalf("SHA512PaddedPair = %x，期望 %s", got512, want512)
	}
}

func TestSRP6aAirplay1(t *testing.T) {
	a := new(big.Int).SetBytes(srpA1)
	s := NewSRP6aAirplay1(a, srpClientID, srpPin, srpB1, srpSalt)
	if !bytes.Equal(s.ClientPK, srp1ClientPK) {
		t.Fatalf("ClientPK 不符:\n got=%x\nwant=%x", s.ClientPK, srp1ClientPK)
	}
	if !bytes.Equal(s.SessionKey, srp1SessionKey) {
		t.Fatalf("SessionKey 不符:\n got=%x\nwant=%x", s.SessionKey, srp1SessionKey)
	}
	if !bytes.Equal(s.ClientProof, srp1ClientProof) {
		t.Fatalf("ClientProof 不符:\n got=%x\nwant=%x", s.ClientProof, srp1ClientProof)
	}
	// SessionKey = SHA1(S||00000000) || SHA1(S||00000001)，40 字节
	if len(s.SessionKey) != 40 {
		t.Fatalf("SessionKey 长度 = %d，期望 40", len(s.SessionKey))
	}
}

func TestSRP6aAirplay2(t *testing.T) {
	a := new(big.Int).SetBytes(srpA2)
	s := NewSRP6aAirplay2(a, srpClientID, srpPin, srpB2, srpSalt)
	if !bytes.Equal(s.ClientPK, srp2ClientPK) {
		t.Fatalf("ClientPK 不符:\n got=%x\nwant=%x", s.ClientPK, srp2ClientPK)
	}
	if !bytes.Equal(s.SessionKey, srp2SessionKey) {
		t.Fatalf("SessionKey 不符:\n got=%x\nwant=%x", s.SessionKey, srp2SessionKey)
	}
	if !bytes.Equal(s.ClientProof, srp2ClientProof) {
		t.Fatalf("ClientProof 不符:\n got=%x\nwant=%x", s.ClientProof, srp2ClientProof)
	}
	if len(s.SessionKey) != 64 {
		t.Fatalf("SessionKey 长度 = %d，期望 64", len(s.SessionKey))
	}
}

// 随机路径：crypto/rand 注入，输出尺寸与正确定性（不比对具体值）。
func TestSRP6aRandom(t *testing.T) {
	s1, err := RandomSRP6aAirplay1(rand.Reader, srpClientID, srpPin, srpB1, srpSalt)
	if err != nil {
		t.Fatal(err)
	}
	if len(s1.ClientPK) != 256 || len(s1.ClientProof) != 20 {
		t.Fatalf("Airplay1 输出尺寸: PK=%d Proof=%d", len(s1.ClientPK), len(s1.ClientProof))
	}
	s2, err := RandomSRP6aAirplay2(rand.Reader, srpClientID, srpPin, srpB2, srpSalt)
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.ClientPK) != 384 || len(s2.ClientProof) != 64 {
		t.Fatalf("Airplay2 输出尺寸: PK=%d Proof=%d", len(s2.ClientPK), len(s2.ClientProof))
	}
	// 相同输入两次（不同随机 a）→ 不同 ClientPK
	other, _ := RandomSRP6aAirplay1(rand.Reader, srpClientID, srpPin, srpB1, srpSalt)
	if bytes.Equal(s1.ClientPK, other.ClientPK) {
		t.Fatal("两次随机应产生不同 ClientPK")
	}
}
