package services

import (
	"bytes"
	"context"
	"encoding/binary"
	"os/exec"
	"path/filepath"
	"songloft/internal/models"
	"testing"
)

func TestChannelRouteRealPCMAndCacheIsolation(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "source.wav")
	// A 7.1 recording with distinct DC values in FL FR FC LFE BL BR SL SR.
	if out, err := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "aevalsrc=0.1|0.2|0.3|0.4|0.5|0.6|0.7|0.8:s=44100:d=0.1:c=7.1", "-c:a", "pcm_s16le", src).CombinedOutput(); err != nil {
		t.Fatalf("fixture %v %s", err, out)
	}
	cs := newLoudnessCacheService(t)
	cs.ffmpegPath.Store(ffmpeg)
	song := &models.Song{ID: 1, Type: models.TypeLocal, FilePath: src, Format: "wav"}
	paths := map[string]bool{}
	for mode, want := range map[string]int{"front_left": 3277, "front_right": 6554, "center": 9830, "lfe": 13107, "back_left": 16384, "back_right": 19661, "side_left": 22938, "side_right": 26214, "back_center": 0} {
		t.Run(mode, func(t *testing.T) {
			path, err := cs.GetOrTranscode(context.Background(), src, song, "wav", 0, -1, false, mode)
			if err != nil {
				t.Fatal(err)
			}
			if path == src || paths[path] {
				t.Fatal("channel cache collided", path)
			}
			paths[path] = true
			pcm, err := exec.Command(ffmpeg, "-v", "error", "-i", path, "-f", "s16le", "pipe:1").Output()
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i+3 < len(pcm); i += 4 {
				l := int(int16(binary.LittleEndian.Uint16(pcm[i:])))
				r := int(int16(binary.LittleEndian.Uint16(pcm[i+2:])))
				if l != r || absRoute(l-want) > 2 {
					t.Fatalf("got %d/%d want %d", l, r, want)
				}
			}
			again, err := cs.GetOrTranscode(context.Background(), src, song, "wav", 0, -1, false, mode)
			if err != nil || again != path {
				t.Fatal("route cache miss", err)
			}
		})
	}
	// Stereo L/R and mono must retain the established downmix semantics.
	for _, mode := range []string{"left", "right", "mono"} {
		filter, _ := ChannelRouteFilter(mode)
		want, err := exec.Command(ffmpeg, "-v", "error", "-i", src, "-af", filter, "-f", "s16le", "pipe:1").Output()
		if err != nil {
			t.Fatal(err)
		}
		path, err := cs.GetOrTranscode(context.Background(), src, song, "wav", 0, -1, false, mode)
		if err != nil {
			t.Fatal(err)
		}
		got, err := exec.Command(ffmpeg, "-v", "error", "-i", path, "-f", "s16le", "pipe:1").Output()
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("stereo route differs", mode, err)
		}
	}
	if path, err := cs.GetOrTranscode(context.Background(), src, song, "wav", 0, -1, false, "stereo"); err != nil || path != src {
		t.Fatal("default stereo unexpectedly modified", err)
	}
	if _, err := cs.GetOrTranscode(context.Background(), src, song, "wav", 0, -1, false, "../bad"); err == nil {
		t.Fatal("invalid route accepted")
	}
}
func absRoute(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
