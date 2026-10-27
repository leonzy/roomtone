package raop

import "encoding/binary"

// encodeALAC packs signed little-endian stereo PCM into an uncompressed ALAC
// element. AirPlay receivers advertising cn=1 require ALAC framing even when
// the samples themselves are uncompressed (FFmpeg libavcodec/alac.c).
func encodeALAC(pcm []byte, channels int) []byte {
	out := make([]byte, (55+len(pcm)*8+3+7)/8)
	bit := 0
	put := func(value uint32, count int) {
		for i := count - 1; i >= 0; i-- {
			out[bit/8] |= byte((value>>i)&1) << (7 - bit%8)
			bit++
		}
	}
	if channels == 2 {
		put(1, 3)
	} else {
		put(0, 3)
	}
	put(0, 4)
	put(0, 12)
	put(1, 1)
	put(0, 2)
	put(1, 1)
	put(uint32(len(pcm)/(2*channels)), 32)
	for i := 0; i+1 < len(pcm); i += 2 {
		put(uint32(binary.LittleEndian.Uint16(pcm[i:])), 16)
	}
	put(7, 3)
	return out
}
