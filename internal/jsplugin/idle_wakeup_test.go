package jsplugin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// idleWakeupJSCode 是一个最小插件：onHTTPRequest 返回可辨识的 JSON 响应体
// {"awake":true,"path":<req.path>}，用于验证休眠插件被唤醒后确实处理了请求。
const idleWakeupJSCode = `
function onInit() {}
function onDeinit() {}
function onHTTPRequest(req) {
    return {
        statusCode: 200,
        headers: {"Content-Type": "application/json"},
        body: JSON.stringify({awake: true, path: req.path})
    };
}
`

// lyricProviderJSCode 是一个歌词提供者插件：/lyric-search 返回可辨识的 LyricPayload。
// 用于验证 SearchLyrics 遍历到被空闲驱逐的歌词插件时，经 InvokeHTTP→EnsureLoaded 唤醒后
// 能真正调用到 onHTTPRequest 并拿到歌词(#303)。
const lyricProviderJSCode = `
function onInit() { songloft.lyrics.registerProvider(); songloft.covers.registerProvider(); }
function onDeinit() { songloft.lyrics.unregisterProvider(); songloft.covers.unregisterProvider(); }
function onHTTPRequest(req) {
    if (req.path === "/disable-search") {
        songloft.lyrics.unregisterProvider(); songloft.covers.unregisterProvider();
        return { statusCode: 200, headers: {}, body: "disabled" };
    }
    if (req.path === "/lyric-search") {
        return {
            statusCode: 200,
            headers: {"Content-Type": "application/json"},
            body: JSON.stringify({lyric: "[00:00.00] woke up and searched"})
        };
    }
    return { statusCode: 404, headers: {}, body: "" };
}
`

// TestIdleWakeup_SearchLyricsWakesSleepingProvider 验证用户核心场景：
// 「本地歌曲无歌词 → SearchLyrics 去插件搜索歌词，若歌词插件已被空闲驱逐（休眠），
// 能否被正常唤醒并完成搜索」(#303)。
//
// 覆盖机制：
//   - 插件注册为歌词提供者（lyricProviders 记录 entryPath）。
//   - 空闲驱逐 → UnloadPlugin 卸 VM，但**故意不清理** lyricProviders，DB status 仍 active。
//   - SearchLyrics 遍历 lyricProviders → InvokeHTTP → EnsureLoaded 同步懒加载唤醒 →
//     dispatchToPlugin 调 onHTTPRequest("/lyric-search") → 返回歌词。
func TestIdleWakeup_SearchLyricsWakesSleepingProvider(t *testing.T) {
	pluginsDir, dataDir, repo, _ := setupTestEnv(t)
	ctx := context.Background()
	const entryPath = "test-lyric-provider"

	manifest := testManifest(entryPath)
	zipData := createTestPluginZip(t, manifest, lyricProviderJSCode)

	zipFileName := entryPath + ".jsplugin.zip"
	if err := os.WriteFile(filepath.Join(pluginsDir, zipFileName), zipData, 0o644); err != nil {
		t.Fatalf("write zip: %v", err)
	}

	plugin := &JSPlugin{
		Name:        manifest.Name,
		Version:     manifest.Version,
		Description: manifest.Description,
		Author:      manifest.Author,
		EntryPath:   manifest.EntryPath,
		Main:        manifest.Main,
		Permissions: manifest.Permissions,
		Status:      JSPluginStatusActive,
		FilePath:    zipFileName,
	}
	if err := repo.Create(ctx, plugin); err != nil {
		t.Fatalf("create plugin record: %v", err)
	}

	manager := NewManager(repo, pluginsDir, dataDir, "", nil, nil)
	t.Cleanup(func() { manager.Close() })

	if err := manager.LoadPlugin(ctx, plugin); err != nil {
		t.Skipf("LoadPlugin failed (may need QuickJS runtime): %v", err)
	}

	// 模拟插件在 onInit 里 registerProvider('lyrics') 的效果：注册为歌词提供者。
	manager.RegisterLyricProvider(entryPath)

	// Exercise the actual health-check eviction path, including onDeinit cleanup.
	// Drain queued initialization before aging the last-activity timestamp.
	if _, _, _, err := manager.InvokeHTTP(ctx, entryPath, "GET", "/ready", nil, nil); err != nil {
		t.Fatal(err)
	}
	svc, _ := manager.GetService(entryPath)
	checker := NewHealthChecker(manager)
	evicted := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		svc.mu.Lock()
		svc.lastActive = time.Now().Add(-11 * time.Minute)
		svc.mu.Unlock()
		if checker.checkIdle(svc) {
			evicted = true
			break
		}
		// Let queued bridge continuations finish; they are real near-term timers
		// and correctly prevent eviction while still pending.
		time.Sleep(10 * time.Millisecond)
	}
	if !evicted {
		t.Fatalf("idle health check must evict the provider: lastActive=%v nextTimer=%v streams=%v processes=%v", svc.LastActive(), manager.jsManager.GetNextTimerDeadline(svc.EnvID()), svc.HasActiveHTTPStreams(), svc.HasRunningProcesses())
	}
	if _, ok := manager.GetService(entryPath); ok {
		t.Fatal("前置条件：空闲驱逐后插件应处于休眠（不在内存）")
	}
	if !manager.HasLyricProvider() {
		t.Fatal("前置条件：驱逐后 lyricProviders 应保留该项（否则永远无法被唤醒搜索）")
	}
	if _, ok := manager.coverProviders.Load(entryPath); !ok {
		t.Fatal("onDeinit must not remove the known cover provider during eviction")
	}

	// 核心断言：搜歌词时休眠插件被同步唤醒并返回歌词。
	payload, err := manager.SearchLyrics(ctx, "Some Song", "Some Artist", "Some Album", 180.0, "", "")
	if err != nil {
		t.Fatalf("SearchLyrics 应成功唤醒休眠插件并返回歌词，却失败: %v", err)
	}
	if payload == nil || payload.IsEmpty() {
		t.Fatalf("期望拿到非空歌词，got %+v", payload)
	}
	if payload.Lyric != "[00:00.00] woke up and searched" {
		t.Errorf("期望拿到插件返回的歌词内容（证明 onHTTPRequest 被真正执行），got %q", payload.Lyric)
	}

	// 唤醒后应留在内存。
	if _, ok := manager.GetService(entryPath); !ok {
		t.Fatal("SearchLyrics 后 GetService 应为 true（休眠歌词插件被同步唤醒并留存）")
	}
	if _, _, _, err := manager.InvokeHTTP(ctx, entryPath, "GET", "/disable-search", nil, nil); err != nil {
		t.Fatal(err)
	}
	if manager.HasLyricProvider() {
		t.Fatal("explicit runtime unregister must remove the provider")
	}
	if _, ok := manager.coverProviders.Load(entryPath); ok {
		t.Fatal("explicit runtime unregister must remove the cover provider")
	}
	if err := manager.ReloadPlugin(ctx, entryPath); err != nil {
		t.Fatal(err)
	}
	if !manager.HasLyricProvider() {
		t.Fatal("reloaded plugin must register again")
	}
	if err := manager.DisablePlugin(ctx, plugin.ID); err != nil {
		t.Fatal(err)
	}
	if manager.HasLyricProvider() {
		t.Fatal("disabled plugin must not remain a provider")
	}
	if _, ok := manager.coverProviders.Load(entryPath); ok {
		t.Fatal("disabled cover provider must be removed")
	}
}

// TestIdleWakeup_HTTPRequestReloadsEvictedPlugin 验证「空闲驱逐（休眠）后，
// 通过 HTTP 访问插件 API 接口能被同步唤醒并正常返回响应」。
//
// 覆盖机制：
//   - 插件空闲 → UnloadPlugin 卸载 VM，但 DB status 仍为 active。
//   - HTTP 入口 handlePluginAPIRequest → EnsureLoaded 同步懒加载（DB active 但内存无 service
//     → LoadPlugin → dispatchToPlugin 调 onHTTPRequest）。
//   - 加载失败按语义映射状态码：inactive → 403。
func TestIdleWakeup_HTTPRequestReloadsEvictedPlugin(t *testing.T) {
	pluginsDir, dataDir, repo, _ := setupTestEnv(t)
	ctx := context.Background()
	const entryPath = "test-idle-wakeup"

	manifest := testManifest(entryPath)
	zipData := createTestPluginZip(t, manifest, idleWakeupJSCode)

	zipFileName := entryPath + ".jsplugin.zip"
	if err := os.WriteFile(filepath.Join(pluginsDir, zipFileName), zipData, 0o644); err != nil {
		t.Fatalf("write zip: %v", err)
	}

	plugin := &JSPlugin{
		Name:        manifest.Name,
		Version:     manifest.Version,
		Description: manifest.Description,
		Author:      manifest.Author,
		EntryPath:   manifest.EntryPath,
		Main:        manifest.Main,
		Permissions: manifest.Permissions,
		Status:      JSPluginStatusActive,
		FilePath:    zipFileName,
	}
	if err := repo.Create(ctx, plugin); err != nil {
		t.Fatalf("create plugin record: %v", err)
	}

	manager := NewManager(repo, pluginsDir, dataDir, "", nil, nil)
	t.Cleanup(func() { manager.Close() })

	// 首次加载：模拟插件正常运行。QuickJS 不可用时按现有测试模式跳过。
	if err := manager.LoadPlugin(ctx, plugin); err != nil {
		t.Skipf("LoadPlugin failed (may need QuickJS runtime): %v", err)
	}

	// 路由与生产一致：RegisterAPIRoutes 注册 catch-all handlePluginAPIRequest。
	// 测试不挂 AuthMiddleware，故无需 token。
	router := chi.NewRouter()
	manager.RegisterAPIRoutes(router)

	apiURL := "/api/v1/jsplugin/" + entryPath + "/api/ping"

	// 子测试 1：加载后在内存；UnloadPlugin 模拟空闲驱逐后不在内存；但 DB status 仍 active。
	t.Run("evict_keeps_db_active", func(t *testing.T) {
		if _, ok := manager.GetService(entryPath); !ok {
			t.Fatal("加载后 GetService 应为 true")
		}

		if err := manager.UnloadPlugin(ctx, entryPath); err != nil {
			t.Fatalf("UnloadPlugin failed: %v", err)
		}

		if _, ok := manager.GetService(entryPath); ok {
			t.Fatal("空闲驱逐后 GetService 应为 false（VM 已卸载）")
		}

		dbPlugin, err := repo.GetByEntryPath(ctx, entryPath)
		if err != nil {
			t.Fatalf("GetByEntryPath: %v", err)
		}
		if dbPlugin.Status != JSPluginStatusActive {
			t.Fatalf("驱逐后 DB status 应仍为 active，got %s", dbPlugin.Status)
		}
	})

	// 子测试 2：HTTP 请求打到休眠插件接口 → 同步唤醒 → 返回插件的正常响应；
	// 请求后 service 又回到内存（证明被拉起并留存）。
	t.Run("http_request_wakes_up_sleeping_plugin", func(t *testing.T) {
		// 前置：确保处于休眠状态（不在内存）。
		if _, ok := manager.GetService(entryPath); ok {
			t.Fatal("前置条件：请求前插件应处于休眠（不在内存）")
		}

		req := httptest.NewRequest(http.MethodGet, apiURL, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("期望被唤醒后返回插件正常码 200，got %d，body=%s", rec.Code, rec.Body.String())
		}

		var body struct {
			Awake bool   `json:"awake"`
			Path  string `json:"path"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("解析插件响应体失败: %v，raw=%s", err, rec.Body.String())
		}
		if !body.Awake {
			t.Errorf("期望插件响应体 awake=true（证明 onHTTPRequest 被真正执行），got %s", rec.Body.String())
		}
		if body.Path != "/api/ping" {
			t.Errorf("期望 req.path=/api/ping（证明路由正确转发），got %q", body.Path)
		}

		// 唤醒后应留在内存。
		if _, ok := manager.GetService(entryPath); !ok {
			t.Fatal("请求后 GetService 应为 true（休眠插件被同步唤醒并留在内存）")
		}
	})

	// 子测试 3：status 改为 inactive 再驱逐 → HTTP 访问应返回 403（只有真实故障/禁用才报错，
	// 不与休眠自愈混淆）。
	t.Run("inactive_plugin_returns_403", func(t *testing.T) {
		if err := repo.UpdateStatus(ctx, plugin.ID, JSPluginStatusInactive); err != nil {
			t.Fatalf("UpdateStatus inactive: %v", err)
		}
		if err := manager.UnloadPlugin(ctx, entryPath); err != nil {
			t.Fatalf("UnloadPlugin failed: %v", err)
		}
		if _, ok := manager.GetService(entryPath); ok {
			t.Fatal("前置条件：inactive 后插件应不在内存")
		}

		req := httptest.NewRequest(http.MethodGet, apiURL, nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("inactive 插件应返回 403，got %d，body=%s", rec.Code, rec.Body.String())
		}

		var errBody struct {
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
			t.Fatalf("解析错误响应体失败: %v，raw=%s", err, rec.Body.String())
		}
		if errBody.Detail != "plugin_disabled" {
			t.Errorf("期望 detail=plugin_disabled，got %q", errBody.Detail)
		}

		// inactive 不应被 HTTP 请求拉起。
		if _, ok := manager.GetService(entryPath); ok {
			t.Error("inactive 插件不应被 HTTP 请求唤醒到内存")
		}
	})
}
