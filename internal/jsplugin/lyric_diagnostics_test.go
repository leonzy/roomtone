package jsplugin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"songloft/internal/models"
	"strings"
	"testing"
)

func TestSearchLyricsPreservesFailureResponse(t *testing.T) {
	pluginsDir, dataDir, repo, _ := setupTestEnv(t)
	manifest := testManifest("failed-lyrics")
	code := `function onInit() {} function onHTTPRequest(req) { return {statusCode:502, headers:{"Content-Type":"application/json"}, body:JSON.stringify({error:"failed",detail:"QQ音乐：HTTP 503；网易云：请求超时"})}; }`
	name := manifest.EntryPath + ".jsplugin.zip"
	if err := os.WriteFile(filepath.Join(pluginsDir, name), createTestPluginZip(t, manifest, code), 0644); err != nil {
		t.Fatal(err)
	}
	plugin := &JSPlugin{Name: manifest.Name, Version: manifest.Version, EntryPath: manifest.EntryPath, Main: manifest.Main, Permissions: manifest.Permissions, Status: JSPluginStatusActive, FilePath: name}
	if err := repo.Create(context.Background(), plugin); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(repo, pluginsDir, dataDir, "", nil, nil)
	t.Cleanup(func() { manager.Close() })
	if err := manager.LoadPlugin(context.Background(), plugin); err != nil {
		t.Fatal(err)
	}
	manager.RegisterLyricProvider(manifest.EntryPath)
	_, err := manager.SearchLyrics(context.Background(), "Song", "Artist", "", 200, "", "")
	var detail *models.LyricSearchError
	if !errors.As(err, &detail) || detail.Status != 502 || !strings.Contains(detail.Error(), "QQ音乐：HTTP 503") {
		t.Fatalf("diagnostics lost: %v", err)
	}
}
