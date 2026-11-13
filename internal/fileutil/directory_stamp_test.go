package fileutil

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDirectoryStampDetectsOverwriteWithUnchangedDirectoryTime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cover.jpg")
	if err := os.WriteFile(path, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(dir)
	first, err := ReadDirectoryStamp(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ReadDirectoryStamp(context.Background(), dir)
	if err != nil || first != second {
		t.Fatal(first, second, err)
	}
	if err := os.WriteFile(path, []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, old.Add(time.Second), old.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	changed, err := ReadDirectoryStamp(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Modified != changed.Modified || first.Hash == changed.Hash {
		t.Fatalf("overwrite was missed: %+v %+v", first, changed)
	}
	if err := os.Rename(path, filepath.Join(dir, "folder.jpg")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	renamed, err := ReadDirectoryStamp(context.Background(), dir)
	if err != nil || renamed.Hash == changed.Hash {
		t.Fatal("rename missed", err)
	}
	if err := os.Remove(filepath.Join(dir, "folder.jpg")); err != nil {
		t.Fatal(err)
	}
	deleted, err := ReadDirectoryStamp(context.Background(), dir)
	if err != nil || deleted.Hash == renamed.Hash {
		t.Fatal("delete missed", err)
	}
}

func TestDirectoryStampCancellationDoesNotReturnUsableManifest(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "a"), []byte("a"), 0600)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadDirectoryStamp(ctx, dir); err == nil {
		t.Fatal("cancelled manifest accepted")
	}
}
