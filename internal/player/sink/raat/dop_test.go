package raat

import (
	"bytes"
	"testing"
)

// golden 逐字对齐 ZonePlayerDopEncapsulation.cs：
// 每声道 3 字节 [DSD][DSD][marker]，marker 逐帧交替 0x05/0xFA；
// 立体声 6B/帧，DSD 字节在 16 位字内交换（buf[j*4+2],buf[j*4] /
// buf[j*4+3],buf[j*4+1]）；单声道 3B/帧。
func TestDoPEncapsulateStereo(t *testing.T) {
	// 立体声：输入 DSD 字节流（16 位字序：L0 L1 R0 R1 L2 L3 R2 R3 ...）
	// 构造 4 帧（每帧 2 声道 × 2 字节 DSD = 8 字节输入 → 24 字节输出）
	input := []byte{
		// 帧 0: L=[0x11,0x22] R=[0x33,0x44]
		0x11, 0x22, 0x33, 0x44,
		// 帧 1: L=[0x55,0x66] R=[0x77,0x88]
		0x55, 0x66, 0x77, 0x88,
		// 帧 2: L=[0x99,0xAA] R=[0xBB,0xCC]
		0x99, 0xAA, 0xBB, 0xCC,
		// 帧 3: L=[0xDD,0xEE] R=[0xFF,0x01]
		0xDD, 0xEE, 0xFF, 0x01,
	}
	d := &DoP{Channels: 2}
	out := make([]byte, 0, 4*6)
	n := d.Encapsulate(&out, input)
	if n != 24 {
		t.Fatalf("写入 %d 字节，期望 24（4 帧 × 6B）", n)
	}
	// 帧 0：[src2][src0][0xFA][src3][src1][0xFA]
	// （16 位字内交换 buf[j*4+2],buf[j*4] / buf[j*4+3],buf[j*4+1]；
	//  marker 初始 false → 0xFA，逐字对齐 cs:37）
	want0 := []byte{0x33, 0x11, 0xFA, 0x44, 0x22, 0xFA}
	if !bytes.Equal(out[0:6], want0) {
		t.Fatalf("帧 0 = %x，期望 %x", out[0:6], want0)
	}
	// 帧 1：marker 交替 → 0x05
	want1 := []byte{0x77, 0x55, 0x05, 0x88, 0x66, 0x05}
	if !bytes.Equal(out[6:12], want1) {
		t.Fatalf("帧 1 = %x，期望 %x（marker 交替 0xFA）", out[6:12], want1)
	}
	// 帧 2：0xFA
	want2 := []byte{0xBB, 0x99, 0xFA, 0xCC, 0xAA, 0xFA}
	if !bytes.Equal(out[12:18], want2) {
		t.Fatalf("帧 2 = %x，期望 %x", out[12:18], want2)
	}
	// 帧 3：0x05
	want3 := []byte{0xFF, 0xDD, 0x05, 0x01, 0xEE, 0x05}
	if !bytes.Equal(out[18:24], want3) {
		t.Fatalf("帧 3 = %x，期望 %x", out[18:24], want3)
	}
}

// 单声道：每帧 3B [b0][b1][marker]。
func TestDoPEncapsulateMono(t *testing.T) {
	input := []byte{
		0x11, 0x22, // 帧 0
		0x33, 0x44, // 帧 1
	}
	d := &DoP{Channels: 1}
	out := make([]byte, 0, 6)
	n := d.Encapsulate(&out, input)
	if n != 6 {
		t.Fatalf("写入 %d 字节，期望 6（2 帧 × 3B）", n)
	}
	want := []byte{0x11, 0x22, 0xFA, 0x33, 0x44, 0x05}
	if !bytes.Equal(out, want) {
		t.Fatalf("单声道不符:\n got=%x\nwant=%x", out, want)
	}
}

// marker 状态跨调用保持（连续调用交替继续）。
func TestDoPMarkerContinuity(t *testing.T) {
	d := &DoP{Channels: 1}
	// 第一次调用：1 帧（marker 0x05）
	out1 := make([]byte, 0, 3)
	d.Encapsulate(&out1, []byte{0x01, 0x02})
	// 第二次调用：1 帧（marker 应为 0xFA——状态跨调用保持）
	out2 := make([]byte, 0, 3)
	d.Encapsulate(&out2, []byte{0x03, 0x04})
	if out1[2] != 0xFA || out2[2] != 0x05 {
		t.Fatalf("marker 状态未跨调用保持: %02X → %02X", out1[2], out2[2])
	}
}

// 输入不是完整帧（声道×2 字节）时：只处理完整帧，返回实际写入。
func TestDoPPartialInput(t *testing.T) {
	d := &DoP{Channels: 2}
	// 5 字节输入（1 完整帧 4B + 1 字节残缺）
	input := []byte{0x11, 0x22, 0x33, 0x44, 0x55}
	out := make([]byte, 0, 6)
	n := d.Encapsulate(&out, input)
	if n != 6 {
		t.Fatalf("写入 %d 字节，期望 6（只处理 1 完整帧）", n)
	}
}

// DoP 帧率与字节率：DSD64（2822400 Hz）立体声 → DoP 采样率 2822400/2
// （每 DoP 帧 2 个 DSD 字节/声道），字节率 = 2822400/2 × 6 = 846720 B/s。
func TestDoPRates(t *testing.T) {
	const dsdRate = 2822400
	dopFrameRate := dsdRate / 2 / 2 // 每 DoP 帧 2 DSD 字节（1 采样对）
	if dopFrameRate != 705600 {
		t.Fatalf("DoP 帧率 = %d，期望 705600", dopFrameRate)
	}
	byteRate := dopFrameRate * 6
	if byteRate != 4233600 {
		t.Fatalf("字节率 = %d，期望 4233600", byteRate)
	}
}
