package fileutil

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"os"
	"path/filepath"
)

// DirectoryStamp hashes a sorted metadata manifest, not audio/image contents.
// Child mtimes are required: overwriting a file need not change the directory mtime.
type DirectoryStamp struct {
	Modified int64  `json:"modified"`
	Hash     string `json:"hash"`
}

type ManifestEntry struct {
	Name string
	Info os.FileInfo
	Link string
}

func StampDirectory(info os.FileInfo, entries []ManifestEntry) DirectoryStamp {
	h := sha256.New()
	writeStampInfo(h, ".", info, "")
	for _, entry := range entries {
		writeStampInfo(h, entry.Name, entry.Info, entry.Link)
	}
	return DirectoryStamp{Modified: info.ModTime().UnixNano(), Hash: hex.EncodeToString(h.Sum(nil))}
}

func writeStampInfo(h hash.Hash, name string, info os.FileInfo, link string) {
	var numbers [24]byte
	binary.LittleEndian.PutUint64(numbers[:8], uint64(info.Size()))
	binary.LittleEndian.PutUint64(numbers[8:16], uint64(info.ModTime().UnixNano()))
	binary.LittleEndian.PutUint64(numbers[16:], uint64(info.Mode()))
	_, _ = h.Write([]byte(name))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(numbers[:])
	_, _ = h.Write([]byte(link))
	_, _ = h.Write([]byte{0})
}

// ReadDirectoryStamp is the fallback for explicit playlist generation outside a scan.
func ReadDirectoryStamp(ctx context.Context, dir string) (DirectoryStamp, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return DirectoryStamp{}, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return DirectoryStamp{}, err
	}
	manifest := make([]ManifestEntry, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return DirectoryStamp{}, err
		}
		path := filepath.Join(dir, entry.Name())
		fi, err := entry.Info()
		if err != nil {
			return DirectoryStamp{}, err
		}
		link := ""
		if entry.Type()&os.ModeSymlink != 0 {
			link, err = os.Readlink(path)
			if err != nil {
				return DirectoryStamp{}, err
			}
			fi, err = os.Stat(path)
			if err != nil {
				return DirectoryStamp{}, err
			}
		}
		manifest = append(manifest, ManifestEntry{Name: entry.Name(), Info: fi, Link: link})
	}
	return StampDirectory(info, manifest), nil
}

type directoryStampsKey struct{}

// WithDirectoryStamps passes a scan's immutable metadata snapshot to its consumers.
func WithDirectoryStamps(ctx context.Context, stamps map[string]DirectoryStamp) context.Context {
	return context.WithValue(ctx, directoryStampsKey{}, stamps)
}

func CachedDirectoryStamp(ctx context.Context, dir string) (DirectoryStamp, bool) {
	stamps, _ := ctx.Value(directoryStampsKey{}).(map[string]DirectoryStamp)
	stamp, ok := stamps[filepath.Clean(dir)]
	if !ok {
		if absolute, err := filepath.Abs(dir); err == nil {
			stamp, ok = stamps[absolute]
		}
	}
	return stamp, ok
}
