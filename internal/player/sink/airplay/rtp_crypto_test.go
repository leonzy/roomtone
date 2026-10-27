package airplay

import (
	"encoding/hex"
	"testing"
)

func TestEncryptFramesNISTVector(t *testing.T) {
	key, _ := hex.DecodeString("2b7e151628aed2a6abf7158809cf4f3c")
	iv, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f")
	plain, _ := hex.DecodeString("6bc1bee22e409f96e93d7e117393172a")
	want := "7649abac8119b246cee98e9b12e9197d"

	enc, err := EncryptFrames(key, iv, plain)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(enc) != want {
		t.Fatalf("密文不符: %s", hex.EncodeToString(enc))
	}
	dec, err := DecryptFrames(key, iv, enc)
	if err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(dec) != hex.EncodeToString(plain) {
		t.Fatalf("解密不还原: %s", hex.EncodeToString(dec))
	}
}

func TestEncryptFramesPreservesClearTail(t *testing.T) {
	key := make([]byte, 16)
	iv := make([]byte, 16)
	out, err := EncryptFrames(key, iv, []byte("abc"))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != "abc" {
		t.Fatalf("尾部应保持明文和原始长度，实际 %x", out)
	}
}

func TestBuildRTPPacketGolden(t *testing.T) {
	pkt := BuildRTPPacket(0x1234, 0xdeadbeef, 0x01020304, true, []byte{0xaa, 0xbb})
	want := "80e01234deadbeef01020304aabb"
	if got := hex.EncodeToString(pkt); got != want {
		t.Fatalf("RTP 包不符:\n got=%s\nwant=%s", got, want)
	}
}

func TestBuildSyncAndTimingPackets(t *testing.T) {
	sync := BuildSyncPacket(1000, 900, NTPTime{Sec: 0x01020304, Frac: 0x05060708})
	want := "80d40007000003e8010203040506070800000384"
	if got := hex.EncodeToString(sync); got != want {
		t.Fatalf("sync 包不符:\n got=%s\nwant=%s", got, want)
	}
	timing := BuildTimingPacket(NTPTime{Sec: 1}, NTPTime{Sec: 2}, NTPTime{Sec: 3})
	if len(timing) != 32 || timing[1] != ControlTiming {
		t.Fatalf("timing 包结构不对: %x", timing)
	}
}
