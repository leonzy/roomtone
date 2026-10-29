package jsplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/mod/semver"
	"songloft/internal/httputil"
)

const (
	registryMaxDepth     = 20
	registryMaxPlugins   = 500
	registryMaxBodyBytes = 2 * 1024 * 1024 // 2 MB
	registryFetchTimeout = 15 * time.Second
	manifestConcurrency  = 8
)

// RegistryConfig 订阅源配置（存储在 config 表中）。
type RegistryConfig struct {
	URL     string `json:"url"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	Token   string `json:"token,omitempty"`
}

// RegistryEntry 解析后的插件条目（内部表示 + API 返回值）。
type RegistryEntry struct {
	Name           string `json:"name,omitempty"`
	EntryPath      string `json:"entry_path,omitempty"`
	Version        string `json:"version,omitempty"`
	Description    string `json:"description,omitempty"`
	Author         string `json:"author,omitempty"`
	Homepage       string `json:"homepage,omitempty"`
	Icon           string `json:"icon,omitempty"`
	DownloadURL    string `json:"download_url,omitempty"`
	UpdateURL      string `json:"update_url,omitempty"`
	MinHostVersion string `json:"min_host_version,omitempty"`
	// SourceURL 记录该条目来自哪个订阅源（配置中的顶层源 URL）。
	// 仅在多源聚合（FetchAndMergeMulti）时填充，用于安装时按源解析 token。
	SourceURL string `json:"source_url,omitempty"`
}

// RegistryJSON 注册表 JSON 顶层结构。
// plugins 是 plugin.json URL 数组。
type RegistryJSON struct {
	Name     string   `json:"name,omitempty"`
	Includes []string `json:"includes,omitempty"`
	Plugins  []string `json:"plugins"`
}

// RegistryService 处理注册表的拉取、递归解析和去重合并。
// 可安全并发复用：内部无共享可变状态（proxyDown 按次调用传递，缓存自带锁）。
type RegistryService struct {
	httpClient *http.Client
	// cache 缓存拉取结果，让翻页/搜索不必重拉整棵注册表树。
	cacheMu sync.Mutex
	cache   map[string]*registryCacheEntry
}

// NewRegistryService 创建 RegistryService。
func NewRegistryService() *RegistryService {
	return &RegistryService{
		httpClient: httputil.NewClient(registryFetchTimeout),
	}
}

// FetchAndMerge 从指定 URL 拉取注册表（含递归 includes），去重合并后返回插件列表。
// token 非空时，所有 HTTP 请求携带 Authorization: Bearer <token> 头。
func (s *RegistryService) FetchAndMerge(ctx context.Context, registryURL string, githubProxy string, token string) ([]RegistryEntry, []string, error) {
	visited := make(map[string]bool)
	var warnings []string

	// proxyDown 记忆本次拉取内 GitHub 代理是否已失效（传输层错误），置位后后续请求
	// 直接直连，避免几十个请求逐个等代理超时。**必须是每次调用的局部状态**：
	// RegistryService 现在是长生命周期单例，挂在结构体上会让代理一次失败后永久
	// 直连（代理恢复也不再尝试），且并发请求之间互相干扰。
	proxyDown := new(atomic.Bool)

	// [1] 递归拉取所有 registry JSON，收集 plugin.json URL
	var pluginURLs []string
	if err := s.fetchRecursive(ctx, registryURL, githubProxy, token, proxyDown, 0, visited, &pluginURLs, &warnings); err != nil {
		return nil, warnings, err
	}

	if len(pluginURLs) > registryMaxPlugins {
		warnings = append(warnings, fmt.Sprintf("plugin count %d exceeds limit %d, truncated", len(pluginURLs), registryMaxPlugins))
		pluginURLs = pluginURLs[:registryMaxPlugins]
	}

	// [2] 并发解析所有 plugin.json URL
	resolved := s.resolveAll(ctx, pluginURLs, githubProxy, token, proxyDown, &warnings)

	// [3] 按 entry_path + 身份去重（高版本优先），保持首次出现顺序（稳定序，避免分页跳序 #302）。
	// 只按 entry_path 去重会让「同名但不同作者」的插件互相吞掉（#339），故键里带上 identity。
	plugins := make(map[string]int) // entry_path+identity -> result 索引
	result := make([]RegistryEntry, 0, len(resolved))
	for _, entry := range resolved {
		if entry.EntryPath == "" || entry.DownloadURL == "" {
			if entry.EntryPath != "" {
				warnings = append(warnings, fmt.Sprintf("plugin %q: no download_url, skipped", entry.EntryPath))
			}
			continue
		}
		key := registryDedupKey(entry.EntryPath, entry.Author, entry.UpdateURL)
		if idx, exists := plugins[key]; exists {
			if CompareVersion(entry.Version, result[idx].Version) > 0 {
				result[idx] = entry
			}
			continue
		}
		plugins[key] = len(result)
		result = append(result, entry)
	}

	return result, warnings, nil
}

// FetchAndMergeMulti 依次拉取多个订阅源并跨源合并去重（高版本优先）。
// 每个源使用其自身的 Token 认证；单个源失败不中断其他源，错误并入 warnings。
// 典型用于「全部」聚合模式：一次展示所有启用源的插件。
func (s *RegistryService) FetchAndMergeMulti(ctx context.Context, sources []RegistryConfig, githubProxy string) ([]RegistryEntry, []string) {
	// 键为 entry_path + 身份，刻意不含源 URL：官方源与社区聚合源经常同时收录同一个插件，
	// 若把源并入键，同一插件会在「全部」模式里重复显示多遍（#339 的反面）。
	merged := make(map[string]int) // entry_path+identity -> result 索引
	result := make([]RegistryEntry, 0)
	var warnings []string

	for _, src := range sources {
		if src.URL == "" {
			continue
		}
		entries, srcWarnings, err := s.FetchAndMerge(ctx, src.URL, githubProxy, src.Token)
		warnings = append(warnings, srcWarnings...)
		if err != nil {
			label := src.Name
			if label == "" {
				label = src.URL
			}
			warnings = append(warnings, fmt.Sprintf("源 %q 拉取失败: %v", label, err))
			continue
		}
		// 保持首次出现顺序（稳定序，避免分页跳序 #302）
		for _, entry := range entries {
			entry.SourceURL = src.URL // 标记来源，供安装时按源解析 token
			key := registryDedupKey(entry.EntryPath, entry.Author, entry.UpdateURL)
			if idx, exists := merged[key]; exists {
				if CompareVersion(entry.Version, result[idx].Version) > 0 {
					result[idx] = entry
				}
				continue
			}
			merged[key] = len(result)
			result = append(result, entry)
		}
	}
	return result, warnings
}

func (s *RegistryService) fetchRecursive(
	ctx context.Context,
	url string,
	githubProxy string,
	token string,
	proxyDown *atomic.Bool,
	depth int,
	visited map[string]bool,
	pluginURLs *[]string,
	warnings *[]string,
) error {
	if depth > registryMaxDepth {
		*warnings = append(*warnings, fmt.Sprintf("includes depth exceeded %d for %s", registryMaxDepth, url))
		return nil
	}

	canonicalURL := strings.TrimRight(url, "/")
	if visited[canonicalURL] {
		return nil
	}
	visited[canonicalURL] = true

	registry, err := s.fetchJSON(ctx, url, githubProxy, token, proxyDown)
	if err != nil {
		if depth == 0 {
			return fmt.Errorf("fetch registry %s: %w", url, err)
		}
		*warnings = append(*warnings, fmt.Sprintf("failed to fetch include %s: %v", url, err))
		return nil
	}

	*pluginURLs = append(*pluginURLs, registry.Plugins...)

	for _, includeURL := range registry.Includes {
		if includeURL == "" {
			continue
		}
		includeToken := ""
		if token != "" && sameHost(url, includeURL) {
			includeToken = token
		}
		if err := s.fetchRecursive(ctx, includeURL, githubProxy, includeToken, proxyDown, depth+1, visited, pluginURLs, warnings); err != nil {
			return err
		}
	}

	return nil
}

// resolveAll 并发拉取所有 plugin.json URL，返回解析后的 RegistryEntry 列表。
func (s *RegistryService) resolveAll(ctx context.Context, pluginURLs []string, githubProxy string, token string, proxyDown *atomic.Bool, warnings *[]string) []RegistryEntry {
	if len(pluginURLs) == 0 {
		return nil
	}

	result := make([]RegistryEntry, len(pluginURLs))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, manifestConcurrency)

	for i, pluginURL := range pluginURLs {
		if pluginURL == "" {
			continue
		}
		wg.Add(1)
		go func(idx int, rawURL string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			entry, err := s.resolvePluginJSON(ctx, rawURL, githubProxy, token, proxyDown)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				*warnings = append(*warnings, fmt.Sprintf("failed to fetch plugin.json %s: %v", rawURL, err))
				return
			}
			result[idx] = entry
		}(i, pluginURL)
	}
	wg.Wait()

	return result
}

// resolvePluginJSON 拉取远程 plugin.json 并映射到 RegistryEntry。
// 如果 plugin.json 中 download_url 为空但 updateUrl 有值，链式拉取 updateUrl 获取 download_url（兼容旧版插件）。
func (s *RegistryService) resolvePluginJSON(ctx context.Context, rawURL string, githubProxy string, token string, proxyDown *atomic.Bool) (RegistryEntry, error) {
	body, err := s.fetchBody(ctx, rawURL, githubProxy, token, proxyDown)
	if err != nil {
		return RegistryEntry{}, err
	}

	var manifest PluginManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return RegistryEntry{}, fmt.Errorf("parse plugin.json: %w", err)
	}

	entry := RegistryEntry{
		Name:           manifest.Name,
		EntryPath:      manifest.EntryPath,
		Version:        manifest.Version,
		Description:    manifest.Description,
		Author:         manifest.Author,
		Homepage:       manifest.Homepage,
		DownloadURL:    manifest.DownloadURL,
		UpdateURL:      manifest.UpdateURL,
		MinHostVersion: manifest.MinHostVersion,
	}

	if manifest.Icon != "" {
		iconBase := httputil.ApplyGithubProxy(rawURL, githubProxy)
		if lastSlash := strings.LastIndex(iconBase, "/"); lastSlash >= 0 {
			// icon 字段解释：
			// - 含 "/"：视作相对 plugin.json 目录的显式路径（如 "frontend/public/icon.svg"），
			//   兼容 vite 等前端脚手架把静态资源放在 frontend/public/ 的仓库布局。
			// - 不含 "/"：视作裸文件名，回落到 "static/<name>"，保留旧插件的向后兼容契约。
			relPath := manifest.Icon
			if !strings.Contains(relPath, "/") {
				relPath = "static/" + relPath
			}
			externalURL := iconBase[:lastSlash+1] + relPath
			entry.Icon = "/api/v1/proxy?url=" + url.QueryEscape(externalURL)
		}
	}

	// 兼容旧版插件：如果 plugin.json 未直接提供 download_url，通过 updateUrl 链式获取
	if entry.DownloadURL == "" && entry.UpdateURL != "" {
		if updateBody, err := s.fetchBody(ctx, entry.UpdateURL, githubProxy, token, proxyDown); err != nil {
			slog.Debug("chain fetch updateUrl failed", "entryPath", entry.EntryPath, "updateUrl", entry.UpdateURL, "error", err)
		} else {
			var updateManifest PluginManifest
			if err := json.Unmarshal(updateBody, &updateManifest); err == nil && updateManifest.DownloadURL != "" {
				entry.DownloadURL = updateManifest.DownloadURL
			}
		}
	}

	slog.Debug("resolved plugin from plugin.json", "entryPath", entry.EntryPath, "version", entry.Version)
	return entry, nil
}

func (s *RegistryService) fetchJSON(ctx context.Context, url string, githubProxy string, token string, proxyDown *atomic.Bool) (*RegistryJSON, error) {
	body, err := s.fetchBody(ctx, url, githubProxy, token, proxyDown)
	if err != nil {
		return nil, err
	}

	var registry RegistryJSON
	if err := json.Unmarshal(body, &registry); err != nil {
		return nil, fmt.Errorf("parse json: %w", err)
	}

	return &registry, nil
}

func (s *RegistryService) fetchBody(ctx context.Context, rawURL string, githubProxy string, token string, proxyDown *atomic.Bool) ([]byte, error) {
	var header http.Header
	if token != "" {
		githubProxy = ""
		header = http.Header{"Authorization": []string{"Bearer " + token}}
	}

	resp, err := httputil.GetWithGithubProxyFallback(ctx, s.httpClient, rawURL, githubProxy,
		httputil.GithubGetOptions{Header: header, ProxyDown: proxyDown})
	if err != nil {
		return nil, fmt.Errorf("http get: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, registryMaxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if len(body) > registryMaxBodyBytes {
		return nil, fmt.Errorf("response body exceeds %d bytes", registryMaxBodyBytes)
	}

	return body, nil
}

// CompareVersion 比较两个版本号，返回 >0 表示 a 更大。
// 支持 semver（1.2.3）和日期格式（2026.6.2），按 dot-separated 数值逐段比较。
func CompareVersion(a, b string) int {
	canonicalA := "v" + strings.TrimPrefix(a, "v")
	canonicalB := "v" + strings.TrimPrefix(b, "v")
	if semver.IsValid(canonicalA) && semver.IsValid(canonicalB) {
		return semver.Compare(canonicalA, canonicalB)
	}

	aParts := strings.Split(a, ".")
	bParts := strings.Split(b, ".")

	maxLen := max(len(aParts), len(bParts))

	for i := range maxLen {
		aVal := 0
		bVal := 0
		if i < len(aParts) {
			aVal, _ = strconv.Atoi(aParts[i])
		}
		if i < len(bParts) {
			bVal, _ = strconv.Atoi(bParts[i])
		}
		if aVal != bVal {
			return aVal - bVal
		}
	}
	return 0
}

// sameHost 判断两个 URL 的 host 是否相同（scheme+host+port）。
func sameHost(a, b string) bool {
	ua, err := url.Parse(a)
	if err != nil {
		return false
	}
	ub, err := url.Parse(b)
	if err != nil {
		return false
	}
	return strings.EqualFold(ua.Host, ub.Host)
}
