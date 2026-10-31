package executable

import (
	"os/exec"
	"strings"
	"testing"
)

func TestEchoPreservesMediaArguments(t *testing.T) {
	for _, name := range []string{"first", "after previous fixture cleanup"} {
		t.Run(name, func(t *testing.T) {
			out, err := exec.Command(Echo(t), "-i", "音乐 folder/song.flac", "-af", "loudnorm=I=-16", "pipe:1").CombinedOutput()
			if err != nil {
				t.Fatalf("echo: %v: %s", err, out)
			}
			if strings.TrimSpace(string(out)) != "-i 音乐 folder/song.flac -af loudnorm=I=-16 pipe:1" {
				t.Fatalf("changed media arguments: %q", out)
			}
		})
	}
}
