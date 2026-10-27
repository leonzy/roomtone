package raop2

import (
	"bytes"
	"testing"
)

// TLV 枚举值逐字来自 TLVType.cs（注意与常见 HAP 示例不同：State=6 而非 1）：
// Method=0, Identifier=1, Salt=2, PublicKey=3, Proof=4,
// EncryptedDataWithTag=5, State=6, Error=7, Signature=10, Flags=19。
func TestTLVConstants(t *testing.T) {
	if TLVMethod != 0 || TLVIdentifier != 1 || TLVSalt != 2 || TLVPublicKey != 3 ||
		TLVProof != 4 || TLVEncryptedDataWithTag != 5 || TLVState != 6 ||
		TLVError != 7 || TLVSignature != 10 || TLVFlags != 19 {
		t.Fatalf("TLV 常量与 TLVType.cs 不符")
	}
}

// 编码：插入序（对齐 C# Dictionary 的实际遍历序）、空值 [type,0]、>255 分段。
func TestTLVEncode(t *testing.T) {
	var tl TLV
	tl.Add(TLVState, []byte{1})
	tl.Add(TLVIdentifier, []byte{1, 2, 3})
	got := tl.Encode()
	want := []byte{
		6, 1, 1, // State=[1]
		1, 3, 1, 2, 3, // Identifier=[1,2,3]
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Encode 不符:\n got=%x\nwant=%x", got, want)
	}

	// 空值 → [type, 0]
	var empty TLV
	empty.Add(TLVState, nil)
	empty.Add(TLVError, []byte{})
	if got, want := empty.Encode(), []byte{6, 0, 7, 0}; !bytes.Equal(got, want) {
		t.Fatalf("空值编码不符: got=%x want=%x", got, want)
	}
}

// >255 字节分段：600 → 255/255/90，段头 [type][len]。
func TestTLVEncodeSegmentation(t *testing.T) {
	val := make([]byte, 600)
	for i := range val {
		val[i] = byte(i % 251)
	}
	var tl TLV
	tl.Add(TLVPublicKey, val)
	got := tl.Encode()
	if len(got) != 606 { // 600 字节 + 3 段头各 2 字节
		t.Fatalf("总长 = %d，期望 606", len(got))
	}
	if got[0] != TLVPublicKey || got[1] != 255 || got[257] != TLVPublicKey || got[258] != 255 || got[514] != TLVPublicKey || got[515] != 90 {
		t.Fatalf("分段头不符: %x %x %x", got[:2], got[256:258], got[512:516])
	}
	if !bytes.Equal(got[2:257], val[:255]) || !bytes.Equal(got[259:514], val[255:510]) || !bytes.Equal(got[516:], val[510:]) {
		t.Fatal("分段内容不符")
	}
}

// 解码：同 type 连续段合并；未知 type 保留；非连续重复取最后一段（C# 字典覆盖语义）。
func TestTLVDecode(t *testing.T) {
	// [6,1,1] [1,3,1,2,3] → State=[1], Identifier=[1,2,3]
	tl, err := DecodeTLV([]byte{6, 1, 1, 1, 3, 1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tl.Get(TLVState), []byte{1}) || !bytes.Equal(tl.Get(TLVIdentifier), []byte{1, 2, 3}) {
		t.Fatalf("解码不符: %v", tl)
	}

	// 分段合并：PublicKey 600 字节拆 3 段
	val := make([]byte, 600)
	for i := range val {
		val[i] = byte(i % 7)
	}
	var src TLV
	src.Add(TLVPublicKey, val)
	tl, err = DecodeTLV(src.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tl.Get(TLVPublicKey), val) {
		t.Fatalf("分段合并不符: %d 字节", len(tl.Get(TLVPublicKey)))
	}

	// 未知 type 保留
	tl, err = DecodeTLV([]byte{99, 2, 0xaa, 0xbb})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tl.Get(99), []byte{0xaa, 0xbb}) {
		t.Fatalf("未知 type 未保留: %v", tl)
	}

	// 非连续重复：后一段覆盖（对齐 C# Dictionary 索引器赋值语义）
	tl, err = DecodeTLV([]byte{6, 1, 0x01, 1, 2, 0x11, 0x22, 6, 1, 0x02})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(tl.Get(TLVState), []byte{0x02}) {
		t.Fatalf("非连续重复应取最后一段: %x", tl.Get(TLVState))
	}
	if !bytes.Equal(tl.Get(TLVIdentifier), []byte{0x11, 0x22}) {
		t.Fatalf("其它 type 不受影响: %x", tl.Get(TLVIdentifier))
	}

	// 空输入 → 空 TLV；空值 → 空切片
	tl, err = DecodeTLV(nil)
	if err != nil || len(tl.keys) != 0 {
		t.Fatalf("空输入应返回空 TLV: %v %v", tl, err)
	}
	tl, err = DecodeTLV([]byte{6, 0})
	if err != nil || len(tl.Get(TLVState)) != 0 {
		t.Fatalf("空值解码不符: %v %v", tl, err)
	}

	// 截断输入报错不 panic
	if _, err := DecodeTLV([]byte{6}); err == nil {
		t.Fatal("截断输入应报错")
	}
	if _, err := DecodeTLV([]byte{6, 3, 0x01}); err == nil {
		t.Fatal("值长度越界应报错")
	}
}
