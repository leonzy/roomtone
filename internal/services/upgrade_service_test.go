package services

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"songloft/internal/models"
	"songloft/internal/version"
)

func TestUpgradePlatformSuffix(t *testing.T) {
	for arch, suffix := range map[string]string{"amd64": "amd64", "arm64": "arm64", "arm": "armv7"} {
		for _, buildType := range []string{"", "full", "lite"} {
			t.Run(arch+"/"+buildType, func(t *testing.T) {
				want := "-linux-" + suffix
				if buildType == "lite" {
					want += "-lite"
				}
				got, err := upgradePlatformSuffix("linux", arch, buildType)
				if err != nil || got != want {
					t.Fatalf("suffix = %q, %v; want %q", got, err, want)
				}
			})
		}
	}
	for _, platform := range [][2]string{{"linux", "386"}, {"linux", "riscv64"}, {"darwin", "arm64"}} {
		if _, err := upgradePlatformSuffix(platform[0], platform[1], "full"); err == nil {
			t.Fatalf("unsupported platform %v should fail", platform)
		}
	}
}

func TestDownloadBinaryUsesRuntimePlatformAndBaseImageBuildType(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Docker upgrades require Linux")
	}
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv7"
	}
	for _, buildType := range []string{"full", "lite"} {
		t.Run(buildType, func(t *testing.T) {
			wantPath := "/songloft-linux-" + arch
			if buildType == "lite" {
				wantPath += "-lite"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != wantPath {
					t.Errorf("download path = %q; want %q", r.URL.Path, wantPath)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				_, _ = w.Write([]byte("release fixture"))
			}))
			defer server.Close()
			svc := NewUpgradeService()
			svc.baseImageBuildTypeOnce.Do(func() { svc.baseImageBuildType = buildType })
			target := filepath.Join(t.TempDir(), "songloft.new")
			if err := svc.DownloadBinary(server.URL+"/songloft", target, ""); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(target)
			if err != nil || string(data) != "release fixture" {
				t.Fatalf("downloaded data = %q, %v", data, err)
			}
		})
	}
}

func TestUpgradeBinaryValidation(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Docker upgrades require Linux ELF binaries")
	}
	dir := t.TempDir()
	source := filepath.Join(dir, "probe.go")
	// 真正的可执行文件，覆盖 exec 失败、非零退出、超时和版本解析。
	if err := os.WriteFile(source, []byte(`package main
import ("fmt"; "os"; "time")
func main() {
    mode := os.Getenv("SONGLOFT_TEST_UPGRADE_MODE")
    if mode == "timeout" { time.Sleep(30 * time.Second) }
    if mode == "invalid-version" { fmt.Println("some other program"); return }
    fmt.Println("Roomtone Version: dev\nBuild Type: full")
    if mode == "exit-error" { os.Exit(2) }
}`), 0600); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(dir, "probe")
	cmd := exec.Command("go", "build", "-o", probe, source)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build probe: %v\n%s", err, output)
	}
	native, err := os.ReadFile(probe)
	if err != nil {
		t.Fatal(err)
	}
	foreign := append([]byte(nil), native...)
	// 替换为另一种机器类型，确保即使装了 binfmt/QEMU 也必须拒绝。
	machine := uint16(183) // EM_AARCH64
	if runtime.GOARCH == "arm64" {
		machine = 62 // EM_X86_64
	}
	binary.LittleEndian.PutUint16(foreign[18:20], machine)

	svc := NewUpgradeService()
	for name, validate := range map[string]func(string) error{"online": svc.TestBinary, "upload": svc.ValidateBinary} {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				name, mode string
				data       []byte
				wantError  bool
			}{
				{name: "valid", data: native},
				{name: "wrong-architecture", data: foreign, wantError: true},
				{name: "html-response", data: []byte("<html>proxy error</html>"), wantError: true},
				{name: "truncated", data: native[:32], wantError: true},
				{name: "nonzero-exit", mode: "exit-error", data: native, wantError: true},
				{name: "invalid-version", mode: "invalid-version", data: native, wantError: true},
				{name: "timeout", mode: "timeout", data: native, wantError: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					t.Setenv("SONGLOFT_TEST_UPGRADE_MODE", tc.mode)
					path := filepath.Join(t.TempDir(), "songloft.new")
					if err := os.WriteFile(path, tc.data, 0600); err != nil {
						t.Fatal(err)
					}
					err := validate(path)
					if (err != nil) != tc.wantError {
						t.Fatalf("validation error = %v; wantError = %v", err, tc.wantError)
					}
					if tc.name == "wrong-architecture" && err != nil && !strings.Contains(err.Error(), "architecture mismatch") {
						t.Fatalf("expected architecture mismatch, got %v", err)
					}
				})
			}
		})
	}
}

func withVersionInfo(t *testing.T, versionValue, gitCommit, buildTime, buildType string) {
	t.Helper()

	oldVersion := version.Version
	oldGitCommit := version.GitCommit
	oldBuildTime := version.BuildTime
	oldBuildType := version.BuildType

	version.Version = versionValue
	version.GitCommit = gitCommit
	version.BuildTime = buildTime
	version.BuildType = buildType

	t.Cleanup(func() {
		version.Version = oldVersion
		version.GitCommit = oldGitCommit
		version.BuildTime = oldBuildTime
		version.BuildType = oldBuildType
	})
}

func TestIsNewerVersionReleaseUsesVersionNumber(t *testing.T) {
	withVersionInfo(t, "2.9.6", "abc123", "2026-07-01_00:00:00", "")

	svc := NewUpgradeService()
	sameVersionDifferentCommit := &models.RemoteVersionInfo{
		Version:   "v2.9.6",
		GitCommit: "def456",
		BuildTime: "2026-07-02_00:00:00",
	}
	if svc.isNewerVersion(versionTypeStable, sameVersionDifferentCommit) {
		t.Fatal("same release version with different commit/build time should not be newer")
	}

	newerVersion := &models.RemoteVersionInfo{
		Version:   "v2.9.7",
		GitCommit: "def456",
		BuildTime: "2026-07-02_00:00:00",
	}
	if !svc.isNewerVersion(versionTypeStable, newerVersion) {
		t.Fatal("higher release version should be newer")
	}
}

func TestIsNewerVersionDevUsesBuildTime(t *testing.T) {
	withVersionInfo(t, "dev", "abc123", "2026-07-01_10:00:00", "lite")

	svc := NewUpgradeService()
	olderBuild := &models.RemoteVersionInfo{
		Version:   "dev",
		GitCommit: "def456",
		BuildTime: "2026-07-01_09:00:00",
	}
	if svc.isNewerVersion(versionTypeDev, olderBuild) {
		t.Fatal("older dev build time should not be newer")
	}

	sameCommitNewerBuild := &models.RemoteVersionInfo{
		Version:   "dev",
		GitCommit: "abc123",
		BuildTime: "2026-07-01_11:00:00",
	}
	if svc.isNewerVersion(versionTypeDev, sameCommitNewerBuild) {
		t.Fatal("same git commit should not be newer even with later build time")
	}

	differentCommitNewerBuild := &models.RemoteVersionInfo{
		Version:   "dev",
		GitCommit: "def456",
		BuildTime: "2026-07-01_11:00:00",
	}
	if !svc.isNewerVersion(versionTypeDev, differentCommitNewerBuild) {
		t.Fatal("different commit with newer build time should be newer")
	}
}

func TestIsNewerVersionRejectsCrossChannel(t *testing.T) {
	withVersionInfo(t, "dev", "abc123", "2026-07-01_10:00:00", "")

	svc := NewUpgradeService()
	stableBuild := &models.RemoteVersionInfo{
		Version:   "v9.9.9",
		GitCommit: "def456",
		BuildTime: "2026-07-01_11:00:00",
	}
	if svc.isNewerVersion(versionTypeStable, stableBuild) {
		t.Fatal("dev runtime should not consider stable release as an update")
	}
	if err := svc.ValidateVersionTypeForUpgrade(versionTypeStable); err == nil {
		t.Fatal("dev runtime should reject stable upgrade request")
	}
}

func TestIsNewerVersionDevUnknownCommitNeverUpdates(t *testing.T) {
	withVersionInfo(t, "dev", "unknown", "unknown", "")

	svc := NewUpgradeService()
	remote := &models.RemoteVersionInfo{
		Version:   "dev",
		GitCommit: "dcd774e",
		BuildTime: "2026-08-31_03:58:49",
	}
	if svc.isNewerVersion(versionTypeDev, remote) {
		t.Fatal("local dev build without ldflags (unknown commit) should never report updates")
	}
}

func TestCurrentBuildTypeDefaultsToFull(t *testing.T) {
	withVersionInfo(t, "2.9.6", "abc123", "2026-07-01_10:00:00", "")

	svc := NewUpgradeService()
	if got := svc.CurrentBuildType(); got != buildTypeFull {
		t.Fatalf("CurrentBuildType() = %q, want %q", got, buildTypeFull)
	}
}
