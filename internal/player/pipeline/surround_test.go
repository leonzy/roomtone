package pipeline

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"songloft/internal/player"
	"songloft/internal/player/sink"
	"testing"
	"time"
)

func TestSurroundDecodeAndDistribution(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	path := filepath.Join(t.TempDir(), "channels.wav")
	cmd := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "aevalsrc=0.01|0.02|0.03|0.04|0.05|0.06|0.07|0.08:s=44100:d=0.1:c=7.1", "-c:a", "pcm_s16le", path)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, b)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	factory := NewFFmpegDecoderFactory(func() string { return ffmpeg }, nil)
	dec, err := factory.Open(ctx, player.Track{Path: path}, 0, sink.Format{SampleRate: 44100, Channels: player.SurroundPCMChannels, Bits: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	pcm, err := io.ReadAll(dec)
	if err != nil {
		t.Fatal(err)
	}
	// FFmpeg 7.1: FL FR FC LFE BL BR SL SR. BC is genuinely absent.
	expected := map[string]int{"front_left": 328, "front_right": 655, "center": 983, "lfe": 1311, "back_left": 1638, "back_right": 1966, "back_center": 0, "side_left": 2294, "side_right": 2621}
	for mode, want := range expected {
		routed := player.RouteSurround(pcm, mode)
		got := int(int16(binary.LittleEndian.Uint16(routed)))
		if got < want-1 || got > want+1 {
			t.Fatalf("%s: got %d want %d", mode, got, want)
		}
		for i := 0; i < len(routed); i += 4 {
			if !bytes.Equal(routed[i:i+2], routed[i+2:i+4]) {
				t.Fatalf("%s not duplicated", mode)
			}
		}
	}
	// The ordinary speaker still receives exactly FFmpeg's own stereo mix.
	stereo, err := exec.Command(ffmpeg, "-v", "error", "-i", path, "-ac", "2", "-ar", "44100", "-f", "s16le", "pipe:1").Output()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(player.RouteSurround(pcm, "stereo"), stereo) {
		t.Fatal("stereo downmix changed")
	}
	a, b := newRecSink("a"), newRecSink("b")
	set := sink.NewSet(sink.Target{PlayerID: "a", Sink: a, ChannelMode: "center"}, sink.Target{PlayerID: "b", Sink: b, ChannelMode: "side_right"})
	clock := newTickClock()
	d := newDistributor(clock, set)
	d.SetPCMChannels(player.SurroundPCMChannels)
	if _, err := d.Run(ctx, bytes.NewReader(pcm)); err != nil {
		t.Fatal(err)
	}
	if a.count() != 5 || b.count() != 5 {
		t.Fatalf("wrong frame count: %d %d", a.count(), b.count())
	}
	if got := int16(binary.LittleEndian.Uint16(a.last())); got < 982 || got > 984 {
		t.Fatal("center lost before sink", got)
	}
	if got := int16(binary.LittleEndian.Uint16(b.last())); got < 2620 || got > 2622 {
		t.Fatal("side right lost before sink", got)
	}
}

// A generated raw DTS fixture exercises the real decoder in CI alongside the
// PCM routing fixture. DTS-ES-in-WAV negotiation has separate user-sample coverage.
func TestDTSDecoderDoesNotDownmixBeforeRouting(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	path := filepath.Join(t.TempDir(), "multichannel.dts")
	cmd := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "aevalsrc=0.01|0.02|0.03|0.04|0.05|0.06:s=44100:d=0.2:c=5.1", "-c:a", "dca", "-strict", "-2", "-f", "dts", path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("DTS encoder fixture: %v %s", err, output)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	factory := NewFFmpegDecoderFactory(func() string { return ffmpeg }, nil)
	decoder, err := factory.Open(ctx, player.Track{Path: path}, 0, sink.Format{SampleRate: 44100, Channels: player.SurroundPCMChannels, Bits: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	pcm, err := io.ReadAll(decoder)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := exec.Command(ffmpeg, "-v", "error", "-i", path, "-af", "pan=mono|c0=FC", "-ar", "44100", "-f", "s16le", "pipe:1").Output()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(reference, make([]byte, len(reference))) {
		t.Fatal("invalid silent center fixture")
	}
	routed := player.RouteSurround(pcm, "center")
	if len(routed) != len(reference)*2 {
		t.Fatal("DTS routed duration changed")
	}
	for i := 0; i < len(reference)/2; i++ {
		if !bytes.Equal(routed[i*4:i*4+2], reference[i*2:i*2+2]) {
			t.Fatalf("DTS center discarded or changed at sample %d", i)
		}
	}
}

// Set ROOMTONE_DTS_SAMPLE to a user-owned file for a full, silent decode.
// No private sample is copied into the repository or sent to a physical sink.
func TestDTSUserSample(t *testing.T) {
	path := os.Getenv("ROOMTONE_DTS_SAMPLE")
	if path == "" {
		t.Skip("ROOMTONE_DTS_SAMPLE not set")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := NewFFmpegDecoderFactory(func() string { return ffmpeg }, nil)
	for _, channels := range []int{2, player.SurroundPCMChannels} {
		dec, err := f.Open(ctx, player.Track{Path: path}, 0, sink.Format{SampleRate: 44100, Channels: channels, Bits: 16})
		if err != nil {
			t.Fatal(err)
		}
		n, readErr := io.Copy(io.Discard, dec)
		closeErr := dec.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("channels %d: %v %v", channels, readErr, closeErr)
		}
		seconds := float64(n) / float64(channels*2*44100)
		if seconds < 240 || seconds > 250 {
			t.Fatalf("decoded duration %.3f", seconds)
		}
		t.Logf("channels=%d PCM bytes=%d duration=%.3fs", channels, n, seconds)
	}
	dec, err := f.Open(ctx, player.Track{Path: path}, 120*time.Second, sink.Format{SampleRate: 44100, Channels: player.SurroundPCMChannels, Bits: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	pcm := make([]byte, 44100*player.SurroundPCMChannels*2)
	if _, err := io.ReadFull(dec, pcm); err != nil {
		t.Fatal(err)
	}
	// Compare every channel against a separate native 6.1 decode. A music
	// recording may intentionally leave its center/LFE silent at this point.
	native, err := f.Open(ctx, player.Track{Path: path}, 120*time.Second, sink.Format{SampleRate: 44100, Channels: 7, Bits: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	reference := make([]byte, 44100*7*2)
	if _, err := io.ReadFull(native, reference); err != nil {
		t.Fatal(err)
	}
	for ch, mode := range []string{"front_left", "front_right", "center", "lfe", "back_center", "side_left", "side_right"} {
		out := player.RouteSurround(pcm, mode)
		for i := 0; i < 44100; i++ {
			if !bytes.Equal(out[i*4:i*4+2], reference[i*14+ch*2:i*14+ch*2+2]) {
				t.Fatalf("DTS channel %s differs at sample %d", mode, i)
			}
		}
	}
	for _, mode := range []string{"back_left", "back_right"} {
		if !bytes.Equal(player.RouteSurround(pcm, mode), make([]byte, 44100*4)) {
			t.Fatalf("absent %s must be silent", mode)
		}
	}
}
