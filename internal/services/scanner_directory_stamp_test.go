package services

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"songloft/internal/database"
	"songloft/internal/fileutil"
)

func TestScanFileStampChecksSizeEvenWhenMTimeWasPreserved(t *testing.T) {
	now := time.Now()
	stored := database.LocalPathInfo{FileModifiedAt: &now, FileSize: 10}
	if scanFileStampMatches(stored, ScanFileStamp{Modified: now, Size: 11}) {
		t.Fatal("size change was skipped")
	}
	if !scanFileStampMatches(stored, ScanFileStamp{Modified: now, Size: 10}) {
		t.Fatal("unchanged file was processed")
	}
}

func TestScanDirectoryStampsIncludeChildrenAndReuseAudioStat(t *testing.T) {
	root := t.TempDir()
	album := filepath.Join(root, "album")
	audio := filepath.Join(album, "a.mp3")
	writeAudio(t, audio)
	scanner := newWalkScanner(root)
	first, err := scanner.ScanFilesWithCue(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := fileutil.ReadDirectoryStamp(context.Background(), album)
	if err != nil {
		t.Fatal(err)
	}
	if first.DirectoryStamps[album] != expected {
		t.Fatal("scanner and fallback disagree")
	}
	info, _ := os.Stat(audio)
	if first.FileStamps[audio].Modified != info.ModTime() || first.FileStamps[audio].Size != info.Size() {
		t.Fatal("missing reusable audio stamp")
	}
	rootInfo, _ := os.Stat(root)
	albumInfo, _ := os.Stat(album)
	stamp := time.Now().Add(-time.Minute)
	if err := os.Chtimes(audio, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(root, rootInfo.ModTime(), rootInfo.ModTime())
	_ = os.Chtimes(album, albumInfo.ModTime(), albumInfo.ModTime())
	next, err := scanner.ScanFilesWithCue(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.DirectoryStamps[album].Hash == next.DirectoryStamps[album].Hash {
		t.Fatal("child modification hidden by stable ancestor times")
	}
}
