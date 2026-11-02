package player

import (
	"io"
	"strings"
	"testing"
)

type prefetchFormatDecoder struct {
	io.Reader
	closed bool
}

func (d *prefetchFormatDecoder) Close() error { d.closed = true; return nil }
func (*prefetchFormatDecoder) Err() error     { return nil }

func TestPrefetchRejectsPCMWidthMismatch(t *testing.T) {
	stereo := Format{SampleRate: 44100, Channels: 2, Bits: 16, Codec: CodecL16}
	surround := stereo
	surround.Channels = SurroundPCMChannels
	for _, same := range []bool{false, true} {
		dec := &prefetchFormatDecoder{Reader: strings.NewReader("pcm")}
		e := NewEngine("group", "test", EngineDeps{})
		e.preload = dec
		e.preloadID = 123
		e.preloadFormat = stereo
		want := surround
		if same {
			want = stereo
		}
		got := e.takePrefetched(123, want)
		if same && (got != dec || dec.closed) {
			t.Fatal("matching prefetch not reused")
		}
		if !same && (got != nil || !dec.closed) {
			t.Fatal("wrong PCM width reused or leaked")
		}
		if e.preload != nil || e.preloadID != 0 {
			t.Fatal("prefetch retained after take")
		}
	}
}
