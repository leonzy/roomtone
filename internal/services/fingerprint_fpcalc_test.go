package services

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// Run against the actual decoder and Chromaprint tool; no music library writes.
func TestFingerprintFPCalcIntegration(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	fpcalc, err := exec.LookPath("fpcalc")
	if err != nil {
		t.Skip("fpcalc not installed")
	}
	if !IsChromaprintAvailable() {
		t.Fatal("installed ffmpeg + fpcalc was not detected")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "sweep.wav")
	run := func(args ...string) {
		t.Helper()
		if out, err := exec.Command(ffmpeg, args...).CombinedOutput(); err != nil {
			t.Fatalf("ffmpeg: %v: %s", err, out)
		}
	}
	run("-v", "error", "-f", "lavfi", "-i", "aevalsrc=sin(2*PI*(440+30*t)*t):s=48000:d=24", "-ac", "2", "-c:a", "pcm_s16le", source)
	reference := func(file string) string {
		t.Helper()
		out, err := exec.Command(fpcalc, "-json", "-length", "120", file).Output()
		// The CLI may return 3 on normal EOF; the production parser checks stderr too.
		var result struct {
			Fingerprint string `json:"fingerprint"`
		}
		if e := json.Unmarshal(out, &result); e != nil || result.Fingerprint == "" {
			t.Fatalf("reference: %v / %v: %s", err, e, out)
		}
		return result.Fingerprint
	}
	for _, tc := range []struct {
		name                 string
		start, end, duration float64
	}{{"whole", 0, 0, 24}, {"cue", 4, 16, 12}, {"last cue", 12, 0, 12}} {
		t.Run(tc.name, func(t *testing.T) {
			fp, duration, err := extractFingerprintWithTools(context.Background(), source, tc.start, tc.end, ffmpeg, fpcalc)
			if err != nil {
				t.Fatal(err)
			}
			if duration != tc.duration {
				t.Fatalf("duration %v, want %v", duration, tc.duration)
			}
			clip := filepath.Join(dir, tc.name+".wav")
			run("-v", "error", "-ss", strconv.FormatFloat(tc.start, 'f', 3, 64), "-i", source, "-t", strconv.FormatFloat(tc.duration, 'f', 3, 64), "-c:a", "pcm_s16le", clip)
			if fp != reference(clip) {
				t.Fatal("fingerprint differs from fpcalc on the exact track segment")
			}
		})
	}
	t.Run("120 second cap", func(t *testing.T) {
		long := filepath.Join(dir, "long.wav")
		run("-v", "error", "-f", "lavfi", "-i", "aevalsrc=sin(2*PI*(440+30*t)*t):s=11025:d=130", "-c:a", "pcm_s16le", long)
		fp, duration, err := extractFingerprintWithTools(context.Background(), long, 0, 0, ffmpeg, fpcalc)
		if err != nil {
			t.Fatal(err)
		}
		if duration != 130 {
			t.Fatalf("lost source duration: %v", duration)
		}
		if fp != reference(long) {
			t.Fatal("sample exceeds the 120 second reference")
		}
	})
	t.Run("invalid audio", func(t *testing.T) {
		invalid := filepath.Join(dir, "broken.mp3")
		if err := os.WriteFile(invalid, []byte("not audio"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := extractFingerprintWithTools(context.Background(), invalid, 0, 0, ffmpeg, fpcalc); err == nil {
			t.Fatal("invalid audio accepted")
		}
	})
	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, _, err := extractFingerprintWithTools(ctx, source, 0, 0, ffmpeg, fpcalc); err == nil {
			t.Fatal("cancelled extraction accepted")
		}
	})
}
