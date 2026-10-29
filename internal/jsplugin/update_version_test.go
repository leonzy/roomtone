package jsplugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckUpdateNeverDowngradesOrReplacesBuildMetadata(t *testing.T) {
	for _, tc := range []struct {
		current, remote string
		want            bool
	}{
		{"0.2.16+roomtone.1", "0.2.16", false},
		{"2.3.1+roomtone.1", "2.3.0", false},
		{"2.3.1+roomtone.1", "2.3.2", true},
		{"2.3.1-beta.1", "2.3.1", true},
		{"2.3.1", "2.3.1-beta.1", false},
		{"2026.10.29", "2026.10.28", false},
	} {
		t.Run(tc.current+"_to_"+tc.remote, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]string{"version": tc.remote, "download_url": "https://example.com/plugin.zip"})
			}))
			defer server.Close()
			pluginsDir, dataDir, repo, _ := setupTestEnv(t)
			pm := NewPackageManager(pluginsDir, dataDir, repo)
			manifest := testManifest("version-check")
			manifest.Version = tc.current
			manifest.UpdateURL = server.URL
			plugin, _, err := pm.InstallFromUpload(createTestPluginZip(t, manifest, simpleJSCode))
			if err != nil {
				t.Fatal(err)
			}
			info, err := pm.CheckUpdate(plugin.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			if info.HasUpdate != tc.want {
				t.Fatalf("update=%v want=%v", info.HasUpdate, tc.want)
			}
		})
	}
}
