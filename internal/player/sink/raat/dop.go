package raat

// DoP（DSD over PCM）封装，逐字移植 ZonePlayerDopEncapsulation.cs。
//
// 每声道 3 字节 [DSD][DSD][marker]：
//   - marker 逐帧交替（_flipper ? 5 : 250——初始 false → 首帧 0xFA，
//     逐字对齐 ZonePlayerDopEncapsulation.cs:37）
//   - 立体声 6B/帧，DSD 字节在 16 位字内交换：
//     buf[j*4+2], buf[j*4] / buf[j*4+3], buf[j*4+1]
//   - 单声道 3B/帧
//
// B11 前置验证结论（实测）：本机 ffmpeg 无 DSD 编码器与 DSF muxer
// （-muxers 无 dsf，-encoders 无 pcm_dsd），无法直出 DSD 字节流——
// DSD 曲目继续走 PCM 路径（停止条件触发，如实记录于
// docs/raat-protocol-notes.md）。DoP 封装层照常交付（golden 测试），
// pipeline 解码器接 DSD 直出待后续（接口就绪）。
type DoP struct {
	// Channels 声道数（1 或 2）。
	Channels int
	// flipper marker 交替状态（true → 0x05，false → 0xFA；初始 false）。
	flipper bool
}

// Encapsulate 把 DSD 字节流封装为 DoP 帧写入 dst，返回写入字节数。
// 输入按声道交错（立体声每帧 4 字节：L0 L1 R0 R1）；残缺帧（不足
// 声道×2 字节）不处理。marker 状态跨调用保持。
func (d *DoP) Encapsulate(dst *[]byte, src []byte) int {
	if d.Channels == 0 {
		d.Channels = 2
	}
	frameIn := d.Channels * 2 // 输入每帧字节数（每声道 2 DSD 字节）
	frameOut := d.Channels * 3
	nFrames := len(src) / frameIn
	start := len(*dst)
	// 扩容
	need := start + nFrames*frameOut
	if cap(*dst) < need {
		buf := make([]byte, start, need)
		copy(buf, *dst)
		*dst = buf
	}
	*dst = (*dst)[:need]
	out := *dst
	written := 0
	for j := 0; j < nFrames; j++ {
		marker := byte(0xFA)
		if d.flipper {
			marker = 0x05
		}
		d.flipper = !d.flipper
		switch d.Channels {
		case 2:
			// 16 位字内交换：buf[j*4+2],buf[j*4] / buf[j*4+3],buf[j*4+1]
			i := j * frameIn
			o := start + j*frameOut
			out[o+0] = src[i+2]
			out[o+1] = src[i+0]
			out[o+2] = marker
			out[o+3] = src[i+3]
			out[o+4] = src[i+1]
			out[o+5] = marker
		default:
			// 单声道：[b0][b1][marker]
			i := j * frameIn
			o := start + j*frameOut
			out[o+0] = src[i+0]
			out[o+1] = src[i+1]
			out[o+2] = marker
		}
		written += frameOut
	}
	return written
}
