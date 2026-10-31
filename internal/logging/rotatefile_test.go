package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRotateWriter_TotalBudgetOnStartup(t *testing.T) {
	stamp := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	withFixedNow(t, stamp)
	dir := t.TempDir()
	for _, name := range []string{"songloft-2026-10-31.1.log", "songloft-2026-10-31.2.log", "songloft-2026-10-31.log"} {
		f, err := os.Create(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.Truncate(40 << 20); err != nil {
			t.Fatal(err)
		}
		f.Close()
		if err := os.Chtimes(filepath.Join(dir, name), stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	w, err := NewRotateWriter(dir, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if w.totalSize > DefaultMaxTotalBytes {
		t.Fatalf("startup total=%d", w.totalSize)
	}
	if _, err := os.Stat(filepath.Join(dir, "songloft-2026-10-31.1.log")); !os.IsNotExist(err) {
		t.Fatal("oldest archive must be removed first")
	}
}

func TestRotateWriter_TotalBudgetDuringWrites(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "songloft-important.log")
	if err := os.WriteFile(other, []byte("unrelated"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := NewRotateWriter(dir, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.maxTotal = 30
	for i := 0; i < 15; i++ {
		if _, err := w.Write([]byte("12345678\n")); err != nil {
			t.Fatal(err)
		}
		var total int64
		files, _ := ListLogFiles(dir)
		for _, file := range files {
			if _, _, valid := parseLogFileOrder(filepath.Base(file)); !valid {
				continue
			}
			info, err := os.Stat(file)
			if err != nil {
				t.Fatal(err)
			}
			total += info.Size()
		}
		if total > w.maxTotal {
			t.Fatalf("write %d: total=%d > %d", i, total, w.maxTotal)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("unrelated file was removed", err)
	}
	if _, err := os.Stat(w.file.Name()); err != nil {
		t.Fatal("active log was unlinked", err)
	}
}

// withFixedNow 临时替换 nowFunc，返回恢复函数。
func withFixedNow(t *testing.T, ts time.Time) {
	t.Helper()
	prev := nowFunc
	nowFunc = func() time.Time { return ts }
	t.Cleanup(func() { nowFunc = prev })
}

func TestRotateWriter_WriteAndList(t *testing.T) {
	dir := t.TempDir()
	withFixedNow(t, time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC))

	w, err := NewRotateWriter(dir, 0, 0)
	if err != nil {
		t.Fatalf("NewRotateWriter: %v", err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if _, err := w.Write([]byte("world\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	files, err := ListLogFiles(dir)
	if err != nil {
		t.Fatalf("ListLogFiles: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("期望 1 个日志文件，得到 %d: %v", len(files), files)
	}
	if !strings.HasSuffix(files[0], "songloft-2026-07-22.log") {
		t.Errorf("文件名不符: %s", files[0])
	}
	content, _ := os.ReadFile(files[0])
	if string(content) != "hello\nworld\n" {
		t.Errorf("内容不符: %q", content)
	}
}

func TestRotateWriter_SizeRotation(t *testing.T) {
	dir := t.TempDir()
	withFixedNow(t, time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC))

	w, err := NewRotateWriter(dir, 10, 0) // 10 字节即轮转
	if err != nil {
		t.Fatalf("NewRotateWriter: %v", err)
	}
	defer w.Close()

	w.Write([]byte("12345678\n")) // 9 字节，主文件
	w.Write([]byte("abcdefgh\n")) // 触发 size 轮转（9+9 > 10），归档旧文件后写新主文件

	files, err := ListLogFiles(dir)
	if err != nil {
		t.Fatalf("ListLogFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("期望 2 个文件（主 + 归档），得到 %d: %v", len(files), files)
	}
	// 归档文件内容更旧，按文件名升序排在主文件之前（.1.log < .log）
	if !strings.Contains(files[0], ".1.log") {
		t.Errorf("期望第一个为归档文件（更旧），得到: %v", files)
	}
	if !strings.HasSuffix(files[1], "songloft-2026-07-22.log") {
		t.Errorf("期望第二个为当日主文件，得到: %v", files)
	}
}

func TestListLogFiles_NaturalSequenceOrder(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"songloft-2026-07-22.log",
		"songloft-2026-07-22.10.log",
		"songloft-2026-07-22.2.log",
		"songloft-2026-07-22.1.log",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	files, err := ListLogFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(files))
	for i, path := range files {
		got[i] = filepath.Base(path)
	}
	want := []string{
		"songloft-2026-07-22.1.log",
		"songloft-2026-07-22.2.log",
		"songloft-2026-07-22.10.log",
		"songloft-2026-07-22.log",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("日志顺序不符：got=%v want=%v", got, want)
	}
}

func TestRotateWriter_DateRollover(t *testing.T) {
	dir := t.TempDir()
	base := time.Date(2026, 7, 22, 23, 59, 0, 0, time.UTC)
	withFixedNow(t, base)

	w, err := NewRotateWriter(dir, 0, 0)
	if err != nil {
		t.Fatalf("NewRotateWriter: %v", err)
	}
	defer w.Close()

	w.Write([]byte("day1\n"))
	withFixedNow(t, base.Add(2*time.Minute)) // 跨天
	w.Write([]byte("day2\n"))

	files, err := ListLogFiles(dir)
	if err != nil {
		t.Fatalf("ListLogFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("跨天应产生 2 个文件，得到 %d: %v", len(files), files)
	}
}

func TestRotateWriter_CleanOld(t *testing.T) {
	dir := t.TempDir()
	// 预置一个 10 天前的旧日志文件
	old := filepath.Join(dir, "songloft-2000-01-01.log")
	if err := os.WriteFile(old, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().AddDate(0, 0, -10)
	os.Chtimes(old, oldTime, oldTime)

	withFixedNow(t, time.Now())
	w, err := NewRotateWriter(dir, 0, 7) // 保留 7 天
	if err != nil {
		t.Fatalf("NewRotateWriter: %v", err)
	}
	defer w.Close()

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("超期日志文件应被清理，但仍存在: %v", err)
	}
}
