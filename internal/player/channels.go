package player

import (
	"encoding/binary"
	"songloft/internal/player/sink"
)

func validChannelMode(mode string) bool {
	switch mode {
	case "", "stereo", "left", "right", "mono", "front_left", "front_right", "center", "lfe", "back_left", "back_right", "back_center", "side_left", "side_right":
		return true
	}
	return false
}

// SurroundPCMChannels is an internal bus, never a device's negotiated format.
// Slots 0..8: FL FR FC LFE BL BR BC SL SR; slots 9..10: FFmpeg stereo downmix.
const SurroundPCMChannels = 11

func memberChannel(members []Member, id string) string {
	if len(members) > 1 {
		for _, member := range members {
			if member.PlayerID == id {
				return member.ChannelMode
			}
		}
	}
	return "stereo"
}

func discreteChannel(mode string) int {
	for i, name := range []string{"front_left", "front_right", "center", "lfe", "back_left", "back_right", "back_center", "side_left", "side_right"} {
		if mode == name {
			return i
		}
	}
	return -1
}

func surroundTargets(targets []sink.Target) bool {
	for _, t := range targets {
		if discreteChannel(t.ChannelMode) >= 0 {
			return true
		}
	}
	return false
}

func decodeFormat(format Format, targets []sink.Target) Format {
	if surroundTargets(targets) {
		format.Channels = SurroundPCMChannels
	}
	return format
}

// RouteSurround reduces the internal bus to stereo before device EQ. Missing
// source channels are silent, never substituted with another speaker's channel.
func RouteSurround(pcm []byte, mode string) []byte {
	frames := len(pcm) / (SurroundPCMChannels * 2)
	out := make([]byte, frames*4)
	ch := discreteChannel(mode)
	for i := 0; i < frames; i++ {
		base := i * SurroundPCMChannels * 2
		l, r := 9, 10
		if ch >= 0 {
			l, r = ch, ch
		}
		copy(out[i*4:i*4+2], pcm[base+l*2:base+l*2+2])
		copy(out[i*4+2:i*4+4], pcm[base+r*2:base+r*2+2])
	}
	return out
}

// channelProcessor operates on each target's private PCM copy, after its EQ.
// L/R selection feeds the chosen source channel to BOTH drivers of that speaker.
// Mono uses a widened intermediate to avoid overflow when summing full scale.
type channelProcessor struct {
	eq   sink.SampleProcessor
	mode string
}

func (p channelProcessor) ProcessInPlace(pcm []byte) {
	if p.eq != nil {
		p.eq.ProcessInPlace(pcm)
	}
	for i := 0; i+3 < len(pcm); i += 4 {
		l := int32(int16(binary.LittleEndian.Uint16(pcm[i:])))
		r := int32(int16(binary.LittleEndian.Uint16(pcm[i+2:])))
		var v int32
		switch p.mode {
		case "left":
			v = l
		case "right":
			v = r
		case "mono":
			v = (l + r) / 2
		default:
			return
		}
		binary.LittleEndian.PutUint16(pcm[i:], uint16(int16(v)))
		binary.LittleEndian.PutUint16(pcm[i+2:], uint16(int16(v)))
	}
}

func groupProcessor(eq sink.SampleProcessor, members []Member, id string) sink.SampleProcessor {
	if len(members) < 2 {
		return eq
	}
	for _, m := range members {
		if m.PlayerID == id && (m.ChannelMode == "left" || m.ChannelMode == "right" || m.ChannelMode == "mono") {
			return channelProcessor{eq: eq, mode: m.ChannelMode}
		}
	}
	return eq
}
