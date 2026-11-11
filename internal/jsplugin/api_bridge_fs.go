package jsplugin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"songloft/internal/fileutil"
	"strings"
)

const maxFSFileSize = 10 << 20 // 10MB

func (h *BridgeHandler) handleFS(action, data string) (string, error) {
	switch action {
	case "fs.readFile":
		return h.fsReadFile(data)
	case "fs.writeFile":
		return h.fsWriteFile(data)
	case "fs.appendFile":
		return h.fsAppendFile(data)
	case "fs.readdir":
		return h.fsReaddir(data)
	case "fs.unlink":
		return h.fsUnlink(data)
	case "fs.exists":
		return h.fsExists(data)
	case "fs.mkdir":
		return h.fsMkdir(data)
	case "fs.stat":
		return h.fsStat(data)
	case "fs.rename":
		return h.fsRename(data)
	default:
		return "", fmt.Errorf("unknown fs action: %s", action)
	}
}

// resolveFSPath 解析插件 FS 桥接的路径，支持三种形式：
//   - "music://xxx" → {music_path}/xxx（需 fs:music 权限）
//   - "/absolute/path" → 绝对路径（需 fs:external 权限 + 在配置的外部目录内）
//   - "relative/path" → {pluginDataDir}/relative/path（需 fs 权限）
func (h *BridgeHandler) resolveFSPath(inputPath string) (string, error) {
	if inputPath == "" {
		return "", fmt.Errorf("path cannot be empty")
	}
	if strings.Contains(inputPath, "..") {
		return "", fmt.Errorf("path cannot contain '..'")
	}

	sep := string(filepath.Separator)

	if strings.HasPrefix(inputPath, "music://") {
		if !CheckPermission(h.permissions, PermFSMusic) {
			return "", fmt.Errorf("requires fs:music permission")
		}
		musicPath := h.getMusicPath()
		if musicPath == "" {
			return "", fmt.Errorf("music_path not configured")
		}
		rel := strings.TrimPrefix(inputPath, "music://")
		return resolveContained(musicPath, rel, sep)
	}

	if filepath.IsAbs(inputPath) {
		if !CheckPermission(h.permissions, PermFSExternal) {
			return "", fmt.Errorf("requires fs:external permission")
		}
		allowedDirs := h.getPluginExternalPaths()
		resolved, err := filepath.Abs(inputPath)
		if err != nil {
			return "", err
		}
		for _, dir := range allowedDirs {
			dirResolved, _ := filepath.Abs(dir)
			if resolved == dirResolved || strings.HasPrefix(resolved, dirResolved+sep) {
				return resolved, nil
			}
		}
		return "", fmt.Errorf("path not in allowed directories")
	}

	if !CheckPermission(h.permissions, PermFS) {
		return "", fmt.Errorf("requires fs permission")
	}
	base := h.pluginDataDir()
	return resolveContained(base, inputPath, sep)
}

// resolveContained 将 rel 解析到 baseDir 下，确保结果不逃出 baseDir。
// 与 routes.go 的 resolveInDir 不同，此函数不做 os.Stat / 目录拒绝检查，
// 因为 fs.readdir / fs.stat / fs.mkdir 的目标可能是目录。
func resolveContained(baseDir, rel, sep string) (string, error) {
	joined := filepath.Join(baseDir, rel)
	absClean, err := filepath.Abs(joined)
	if err != nil {
		return "", err
	}
	baseDirResolved, _ := filepath.Abs(baseDir)
	if absClean != baseDirResolved && !strings.HasPrefix(absClean, baseDirResolved+sep) {
		return "", fmt.Errorf("path escapes allowed directory")
	}
	return absClean, nil
}

func (h *BridgeHandler) getMusicPath() string {
	cfg, err := h.db.ConfigRepository().Get(context.Background(), "music_path")
	if err != nil {
		return ""
	}
	var data struct {
		Path string `json:"path"`
	}
	if json.Unmarshal([]byte(cfg.Value), &data) != nil {
		return cfg.Value
	}
	return data.Path
}

func (h *BridgeHandler) getPluginExternalPaths() []string {
	return h.service.plugin.ExternalPaths
}

func (h *BridgeHandler) fsReadFile(data string) (string, error) {
	var req struct {
		Path     string `json:"path"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal([]byte(data), &req); err != nil {
		return "", fmt.Errorf("fs.readFile: %w", err)
	}

	absPath, err := h.resolveFSPath(req.Path)
	if err != nil {
		return "", fmt.Errorf("fs.readFile: %w", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return "", fmt.Errorf("fs.readFile: %w", err)
	}
	if info.Size() > maxFSFileSize {
		return "", fmt.Errorf("fs.readFile: file exceeds %dMB limit", maxFSFileSize>>20)
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		return "", fmt.Errorf("fs.readFile: %w", err)
	}

	if req.Encoding == "base64" {
		return base64.StdEncoding.EncodeToString(content), nil
	}
	return string(content), nil
}

func (h *BridgeHandler) fsWriteFile(data string) (string, error) {
	var req struct {
		Path     string `json:"path"`
		Data     string `json:"data"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal([]byte(data), &req); err != nil {
		return "", fmt.Errorf("fs.writeFile: %w", err)
	}

	absPath, err := h.resolveFSPath(req.Path)
	if err != nil {
		return "", fmt.Errorf("fs.writeFile: %w", err)
	}

	var content []byte
	if req.Encoding == "base64" {
		// base64 编码膨胀系数约 4/3，解码前预检避免先分配大内存再检查
		if len(req.Data) > maxFSFileSize*4/3+4 {
			return "", fmt.Errorf("fs.writeFile: data exceeds %dMB limit", maxFSFileSize>>20)
		}
		content, err = base64.StdEncoding.DecodeString(req.Data)
		if err != nil {
			return "", fmt.Errorf("fs.writeFile: invalid base64: %w", err)
		}
	} else {
		content = []byte(req.Data)
	}

	if len(content) > maxFSFileSize {
		return "", fmt.Errorf("fs.writeFile: data exceeds %dMB limit", maxFSFileSize>>20)
	}

	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("fs.writeFile: mkdir: %w", err)
	}

	if err := os.WriteFile(absPath, content, 0o644); err != nil {
		return "", fmt.Errorf("fs.writeFile: %w", err)
	}
	return "", nil
}

func (h *BridgeHandler) fsAppendFile(data string) (string, error) {
	var req struct {
		Path     string `json:"path"`
		Data     string `json:"data"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal([]byte(data), &req); err != nil {
		return "", fmt.Errorf("fs.appendFile: %w", err)
	}

	absPath, err := h.resolveFSPath(req.Path)
	if err != nil {
		return "", fmt.Errorf("fs.appendFile: %w", err)
	}

	var content []byte
	if req.Encoding == "base64" {
		content, err = base64.StdEncoding.DecodeString(req.Data)
		if err != nil {
			return "", fmt.Errorf("fs.appendFile: invalid base64: %w", err)
		}
	} else {
		content = []byte(req.Data)
	}

	dir := filepath.Dir(absPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("fs.appendFile: mkdir: %w", err)
	}

	// 检查追加后是否超出文件大小限制
	var existingSize int64
	if info, statErr := os.Stat(absPath); statErr == nil {
		existingSize = info.Size()
	} else if !os.IsNotExist(statErr) {
		return "", fmt.Errorf("fs.appendFile: stat: %w", statErr)
	}
	if existingSize+int64(len(content)) > maxFSFileSize {
		return "", fmt.Errorf("fs.appendFile: file size would exceed limit: existing %d + append %d > max %d",
			existingSize, len(content), maxFSFileSize)
	}

	f, err := os.OpenFile(absPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", fmt.Errorf("fs.appendFile: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(content); err != nil {
		return "", fmt.Errorf("fs.appendFile: %w", err)
	}
	return "", nil
}

func (h *BridgeHandler) fsReaddir(data string) (string, error) {
	var req struct {
		Path      string `json:"path"`
		WithStats bool   `json:"withStats"`
	}
	if err := json.Unmarshal([]byte(data), &req); err != nil {
		return "", fmt.Errorf("fs.readdir: %w", err)
	}

	absPath, err := h.resolveFSPath(req.Path)
	if err != nil {
		return "", fmt.Errorf("fs.readdir: %w", err)
	}

	entries, err := os.ReadDir(absPath)
	if err != nil {
		return "", fmt.Errorf("fs.readdir: %w", err)
	}

	type dirEntry struct {
		Name    string `json:"name"`
		IsDir   bool   `json:"isDir"`
		Size    *int64 `json:"size,omitempty"`
		ModTime *int64 `json:"modTime,omitempty"`
	}
	result := make([]dirEntry, 0, len(entries))
	for _, e := range entries {
		entry := dirEntry{Name: e.Name(), IsDir: e.IsDir()}
		// Optional metadata batches file stats into one bridge round trip. Do not
		// follow symlinks here; fs.stat must apply its normal path permission check.
		if req.WithStats && e.Type()&os.ModeSymlink == 0 {
			if info, err := e.Info(); err == nil {
				size, modified := info.Size(), info.ModTime().UnixMilli()
				entry.Size, entry.ModTime = &size, &modified
			}
		}
		result = append(result, entry)
	}

	out, _ := json.Marshal(result)
	return string(out), nil
}

func (h *BridgeHandler) fsUnlink(data string) (string, error) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(data), &req); err != nil {
		return "", fmt.Errorf("fs.unlink: %w", err)
	}

	absPath, err := h.resolveFSPath(req.Path)
	if err != nil {
		return "", fmt.Errorf("fs.unlink: %w", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return "", fmt.Errorf("fs.unlink: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("fs.unlink: cannot unlink directory, use rmdir or remove via readdir")
	}

	if err := os.Remove(absPath); err != nil {
		return "", fmt.Errorf("fs.unlink: %w", err)
	}
	return "", nil
}

func (h *BridgeHandler) fsExists(data string) (string, error) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(data), &req); err != nil {
		return "", fmt.Errorf("fs.exists: %w", err)
	}

	absPath, err := h.resolveFSPath(req.Path)
	if err != nil {
		return "", fmt.Errorf("fs.exists: %w", err)
	}

	_, err = os.Stat(absPath)
	if err != nil {
		return "false", nil
	}
	return "true", nil
}

func (h *BridgeHandler) fsMkdir(data string) (string, error) {
	var req struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	if err := json.Unmarshal([]byte(data), &req); err != nil {
		return "", fmt.Errorf("fs.mkdir: %w", err)
	}

	absPath, err := h.resolveFSPath(req.Path)
	if err != nil {
		return "", fmt.Errorf("fs.mkdir: %w", err)
	}

	if req.Recursive {
		err = os.MkdirAll(absPath, 0o755)
	} else {
		err = os.Mkdir(absPath, 0o755)
	}
	if err != nil {
		return "", fmt.Errorf("fs.mkdir: %w", err)
	}
	return "", nil
}

func (h *BridgeHandler) fsStat(data string) (string, error) {
	var req struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(data), &req); err != nil {
		return "", fmt.Errorf("fs.stat: %w", err)
	}

	absPath, err := h.resolveFSPath(req.Path)
	if err != nil {
		return "", fmt.Errorf("fs.stat: %w", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		return "", fmt.Errorf("fs.stat: %w", err)
	}

	result, _ := json.Marshal(struct {
		Size    int64 `json:"size"`
		ModTime int64 `json:"modTime"`
		IsDir   bool  `json:"isDir"`
	}{
		Size:    info.Size(),
		ModTime: info.ModTime().UnixMilli(),
		IsDir:   info.IsDir(),
	})
	return string(result), nil
}

func (h *BridgeHandler) fsRename(data string) (string, error) {
	var req struct {
		OldPath string `json:"oldPath"`
		NewPath string `json:"newPath"`
	}
	if err := json.Unmarshal([]byte(data), &req); err != nil {
		return "", fmt.Errorf("fs.rename: %w", err)
	}

	absOld, err := h.resolveFSPath(req.OldPath)
	if err != nil {
		return "", fmt.Errorf("fs.rename: oldPath: %w", err)
	}
	absNew, err := h.resolveFSPath(req.NewPath)
	if err != nil {
		return "", fmt.Errorf("fs.rename: newPath: %w", err)
	}

	newDir := filepath.Dir(absNew)
	if err := os.MkdirAll(newDir, 0o755); err != nil {
		return "", fmt.Errorf("fs.rename: mkdir: %w", err)
	}

	if err := fileutil.MoveFile(absOld, absNew); err != nil {
		return "", fmt.Errorf("fs.rename: %w", err)
	}
	return "", nil
}
