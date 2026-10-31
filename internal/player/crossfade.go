package player

import (
	"encoding/binary"
	"io"
	"math"
)

// CrossfadeDefaultSeconds 默认淡化时长（秒）。0 表示关闭。
const CrossfadeDefaultSeconds = 0.0

// crossfadeReader 只输出当前曲，将它的尾部与下一曲的开头混合。
//
// 做法：读取当前曲时始终扣住最后 fadeBytes 字节不输出，到 EOF 时把这段尾巴
// 与下一曲的开头按等功率曲线（cos/sin）交叉混合，然后返回 EOF。
// 下一曲的剩余部分由引擎在更新队列、曲目信息和时间轴后播放；否则整首下一曲
// 都会挂在上一曲名下，之后还会重复播放。输出长度等于当前曲剩余长度。
//
// 引擎重新打开下一曲解码器，但支持 Continue 的输出保持同一传输会话，
// 避免关闭会话时丢掉设备中尚未播放的混合音频。
type crossfadeReader struct {
	cur  io.Reader
	next func() (io.ReadCloser, error)

	frameBytes int
	fadeBytes  int
	// hold 是扣住未输出的当前曲尾巴（最多 fadeBytes）。
	hold []byte
	// pending 是混合后待输出的字节。
	pending []byte

	nextR              io.ReadCloser
	curEnded           bool
	mixing             bool
	mixed              bool
	finished           bool
	consumedNextFrames int
}

// newCrossfadeReader 构造交叉淡化读取器。fade <= 0 或帧参数非法时退化为直读。
func newCrossfadeReader(cur io.Reader, fadeFrames int, frameBytes int, next func() (io.ReadCloser, error)) *crossfadeReader {
	if frameBytes <= 0 {
		frameBytes = 4
	}
	fadeBytes := 0
	if fadeFrames > 0 {
		fadeBytes = fadeFrames * frameBytes
	}
	return &crossfadeReader{
		cur:        cur,
		next:       next,
		frameBytes: frameBytes,
		fadeBytes:  fadeBytes,
	}
}

// Mixed 是否真的做过混合（用于决定下一曲是否跳过已消耗的头部）。
func (r *crossfadeReader) Mixed() bool { return r.mixed }

// ConsumedNextFrames is the actual prefix mixed, including short-track cases.
func (r *crossfadeReader) ConsumedNextFrames() int { return r.consumedNextFrames }

// Read 实现 io.Reader。
func (r *crossfadeReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.finished {
		return 0, io.EOF
	}
	if len(r.pending) > 0 {
		n := copy(p, r.pending)
		r.pending = r.pending[n:]
		return n, nil
	}
	if r.mixing {
		r.finished = true
		return 0, io.EOF
	}
	return r.readCur(p)
}

// readCur 读当前曲，但始终扣住 fadeBytes 不吐。
func (r *crossfadeReader) readCur(p []byte) (int, error) {
	if r.fadeBytes == 0 {
		return r.cur.Read(p)
	}
	for {
		if len(r.hold) > r.fadeBytes {
			n := len(r.hold) - r.fadeBytes
			if n > len(p) {
				n = len(p)
			}
			copy(p, r.hold[:n])
			r.hold = r.hold[n:]
			return n, nil
		}
		if r.curEnded {
			if err := r.startMix(); err != nil {
				return 0, err
			}
			return r.Read(p)
		}
		tmp := make([]byte, len(p))
		n, err := r.cur.Read(tmp)
		if n > 0 {
			r.hold = append(r.hold, tmp[:n]...)
		}
		if err == io.EOF {
			// Read may return data and EOF together: drain the non-overlap prefix first.
			r.curEnded = true
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			// 读不到数据又没 EOF：避免空转。
			return 0, io.ErrNoProgress
		}
	}
}

// startMix 取下一曲并把 hold 与之等功率混合。
func (r *crossfadeReader) startMix() error {
	tail := r.hold
	r.hold = nil
	if len(tail) == 0 {
		r.mixing = true
		return nil
	}
	if r.next == nil {
		r.pending = tail
		r.mixing = true
		return nil
	}
	nc, err := r.next()
	if err != nil {
		// 下一曲打不开：把尾巴原样吐出去，播放不中断（退化成普通切歌）。
		r.pending = tail
		r.mixing = true
		return nil
	}
	r.nextR = nc
	head := make([]byte, len(tail))
	// 下一曲比淡化窗还短：能读到多少混多少，没读到的部分按当前曲原声输出。
	if n, _ := io.ReadFull(nc, head); n < len(head) {
		head = head[:n/r.frameBytes*r.frameBytes]
	}
	r.consumedNextFrames = len(head) / r.frameBytes
	mixed := append([]byte(nil), tail...)
	equalPowerMix(mixed, tail, head, 0, len(tail)/r.frameBytes)
	r.pending = mixed
	r.mixed = r.consumedNextFrames > 0
	r.mixing = true
	return nil
}

// Close 关掉交叉淡化的下一曲解码器（当前曲由引擎自己关）。
func (r *crossfadeReader) Close() error {
	if r.nextR != nil {
		err := r.nextR.Close()
		r.nextR = nil
		return err
	}
	return nil
}

// equalPowerMix 把 out = a·cos(θ) + b·sin(θ) 写入 dst（s16le 双声道交织）。
//
// 等功率而不是线性：线性淡入淡出在交界中点会掉约 3dB，听起来像"凹一下"。
// startFrame/totalFrames 描述这段尾巴在整段淡化窗里的位置——短曲尾巴不足
// 一个窗口时从窗口开头算起，曲线依然单调。
func equalPowerMix(dst, a, b []byte, startFrame, totalFrames int) {
	n := len(dst)
	if len(a) < n {
		n = len(a)
	}
	if len(b) < n {
		n = len(b)
	}
	if totalFrames <= 0 {
		totalFrames = 1
	}
	samples := n / 2
	startSample := startFrame * 2
	totalSamples := totalFrames * 2
	for i := 0; i < samples; i++ {
		t := (float64(startSample+i) + 0.5) / float64(totalSamples)
		if t > 1 {
			t = 1
		}
		outGain := math.Cos(t * math.Pi / 2)
		inGain := math.Sin(t * math.Pi / 2)
		av := int32(int16(binary.LittleEndian.Uint16(a[i*2:])))
		bv := int32(int16(binary.LittleEndian.Uint16(b[i*2:])))
		v := int32(float64(av)*outGain + float64(bv)*inGain)
		if v > math.MaxInt16 {
			v = math.MaxInt16
		} else if v < math.MinInt16 {
			v = math.MinInt16
		}
		binary.LittleEndian.PutUint16(dst[i*2:], uint16(int16(v)))
	}
}

// crossfadeFrames 把淡化时长换算成帧数（向下取整）。
func crossfadeFrames(seconds float64, rate int) int {
	if seconds <= 0 || rate <= 0 {
		return 0
	}
	return int(seconds * float64(rate))
}
