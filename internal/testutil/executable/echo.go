// Package executable supplies portable subprocess fixtures for media tests.
package executable

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

var windowsEcho struct {
	sync.Once
	data []byte
	err  error
}

// Echo returns an executable that prints its arguments without invoking a shell.
// Unix uses echo; Windows builds a tiny Go fixture once and copies its bytes to
// each test's own directory, so cleanup never invalidates another test's path.
func Echo(t testing.TB) string {
	t.Helper()
	if runtime.GOOS != "windows" {
		path, err := exec.LookPath("echo")
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
	windowsEcho.Do(func() {
		dir, err := os.MkdirTemp("", "roomtone-test-echo-")
		if err != nil {
			windowsEcho.err = err
			return
		}
		defer os.RemoveAll(dir)
		src := filepath.Join(dir, "echo.go")
		err = os.WriteFile(src, []byte("package main\nimport (\"fmt\";\"os\";\"strings\")\nfunc main(){fmt.Println(strings.Join(os.Args[1:],\" \"))}\n"), 0600)
		if err != nil {
			windowsEcho.err = err
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		binary := filepath.Join(dir, "echo.exe")
		cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go.exe"), "build", "-o", binary, src)
		if out, err := cmd.CombinedOutput(); err != nil {
			windowsEcho.err = fmt.Errorf("build echo fixture: %w: %s", err, out)
			return
		}
		windowsEcho.data, windowsEcho.err = os.ReadFile(binary)
	})
	if windowsEcho.err != nil {
		t.Fatal(windowsEcho.err)
	}
	path := filepath.Join(t.TempDir(), "echo.exe")
	if err := os.WriteFile(path, windowsEcho.data, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}
