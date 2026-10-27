package airplay

import (
	"encoding/hex"
	"testing"
)

// golden 字节逐字对齐 Roon DMAPEncoder.cs：
// 容器 = 4B tag + 4B BE 长度回填；string = 4B tag + 4B BE 长度 + UTF-8；
// byte = 4B tag + 4B BE(1) + 1 字节。
func TestDMAPEncoderGolden(t *testing.T) {
	var e DMAPEncoder
	e.Begin("mlit")
	e.WriteU8("mikd", 2)
	e.WriteString("asal", "Album")
	e.WriteString("asar", "Artist")
	e.WriteString("asaa", "Artist")
	e.WriteString("minm", "Title")
	e.End("mlit")
	got := hex.EncodeToString(mustDMAPBytes(t, &e))
	// mlit 内容长 = mikd(9) + asal(13) + asar(14) + asaa(14) + minm(13) = 63 = 0x3F
	want := "6d6c6974" + "0000003f" + // mlit
		"6d696b64" + "00000001" + "02" + // mikd = 2
		"6173616c" + "00000005" + "416c62756d" + // asal = "Album"
		"61736172" + "00000006" + "417274697374" + // asar = "Artist"
		"61736161" + "00000006" + "417274697374" + // asaa = "Artist"
		"6d696e6d" + "00000005" + "5469746c65" // minm = "Title"
	if got != want {
		t.Fatalf("DMAP golden 不符:\n got=%s\nwant=%s", got, want)
	}
}

// 嵌套容器：内层长度先回填，外层长度包含内层全部字节。
func TestDMAPEncoderNested(t *testing.T) {
	var e DMAPEncoder
	e.Begin("adbm")
	e.Begin("mlit")
	e.WriteString("minm", "x")
	e.End("mlit")
	e.End("adbm")
	got := hex.EncodeToString(mustDMAPBytes(t, &e))
	// mlit 内容 = minm(4+4+1=9)；adbm 内容 = mlit 整段(4+4+9=17)
	want := "6164626d" + "00000011" +
		"6d6c6974" + "00000009" +
		"6d696e6d" + "00000001" + "78"
	if got != want {
		t.Fatalf("DMAP 嵌套不符:\n got=%s\nwant=%s", got, want)
	}
}

// UTF-8 多字节字符串按字节计长。
func TestDMAPEncoderUTF8(t *testing.T) {
	var e DMAPEncoder
	e.Begin("mlit")
	e.WriteString("minm", "夜曲") // 6 字节
	e.End("mlit")
	got := hex.EncodeToString(mustDMAPBytes(t, &e))
	// mlit 内容 = minm 整段(4+4+6=14)
	want := "6d6c6974" + "0000000e" + "6d696e6d" + "00000006" + hex.EncodeToString([]byte("夜曲"))
	if got != want {
		t.Fatalf("DMAP UTF-8 不符:\n got=%s\nwant=%s", got, want)
	}
}

func TestDMAPEncoderErrors(t *testing.T) {
	// tag 必须恰好 4 字节
	var e DMAPEncoder
	e.Begin("mlit")
	if err := e.End("mikd"); err == nil {
		t.Fatal("End 不匹配应当报错")
	}
	// 未闭合就取字节应当报错
	if _, err := e.Bytes(); err == nil {
		t.Fatal("未闭合的容器取字节应当报错")
	}
	// tag 长度非法
	if err := e.Begin("abc"); err == nil {
		t.Fatal("3 字节 tag 应当报错")
	}
	if err := e.WriteString("abcde", "x"); err == nil {
		t.Fatal("5 字节 tag 应当报错")
	}
	if err := e.WriteU8("abc", 1); err == nil {
		t.Fatal("3 字节 tag 应当报错")
	}
}

func mustDMAPBytes(t *testing.T, e *DMAPEncoder) []byte {
	t.Helper()
	b, err := e.Bytes()
	if err != nil {
		t.Fatalf("DMAPEncoder.Bytes: %v", err)
	}
	return b
}
