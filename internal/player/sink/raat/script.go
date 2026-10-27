package raat

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SignProxy raat-sign 签名代理（对齐 RaatSignProxy.cs）。
//
//	POST {BaseURL}/sign  body {"contentHashes":[sha256hex], "keypairNames":[名]}
//	响应 {"contentHashes": {hash: {keypair: signature}}}
//
// 签名按 `<sha256>:<keypair>` 缓存到本地文件（CachePath，默认
// ~/.config/roomtone/raat-sign-cache.json）。notFound（404）返回明确错误
// （对齐 RaatSignProxy.cs:107-110 的 "signing server returned notFound"）。
type SignProxy struct {
	// BaseURL 默认 https://raat-sign.roonlabs.net/
	BaseURL string
	// CachePath 签名缓存文件路径。
	CachePath string
	// HTTP HTTP 客户端（测试注入）。
	HTTP *http.Client

	mu    sync.Mutex
	cache map[string]string // "<sha256>:<keypair>" -> signature
}

// DefaultSignBaseURL 默认签名服务地址（逐字对齐 RaatSignProxy.cs:174）
const DefaultSignBaseURL = "https://raat-sign.roonlabs.net/"

// DefaultSignCachePath 默认缓存路径。
func DefaultSignCachePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "raat-sign-cache.json"
	}
	return filepath.Join(home, ".config", "roomtone", "raat-sign-cache.json")
}

// loadCache 加载缓存文件（惰性，一次）。
func (p *SignProxy) loadCache() {
	if p.cache != nil {
		return
	}
	p.cache = map[string]string{}
	path := p.CachePath
	if path == "" {
		path = DefaultSignCachePath()
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return // 无缓存文件（首次）——空表
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return // 损坏的缓存——当空表
	}
	p.cache = m
}

// saveCache 写缓存文件（尽力）。
func (p *SignProxy) saveCache() {
	path := p.CachePath
	if path == "" {
		path = DefaultSignCachePath()
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	data, err := json.Marshal(p.cache)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o600)
}

// Sign 取脚本内容的签名。命中缓存直接返回；否则 POST /sign。
func (p *SignProxy) Sign(ctx context.Context, script, keypair string) (string, error) {
	sum := sha256.Sum256([]byte(script))
	hash := hex.EncodeToString(sum[:])
	key := hash + ":" + keypair

	p.mu.Lock()
	p.loadCache()
	if sig, ok := p.cache[key]; ok {
		p.mu.Unlock()
		return sig, nil
	}
	p.mu.Unlock()

	baseURL := p.BaseURL
	if baseURL == "" {
		baseURL = DefaultSignBaseURL
	}
	client := p.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	body, err := json.Marshal(map[string]any{
		"contentHashes": []string{hash},
		"keypairNames":  []string{keypair},
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/sign", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("raat-sign: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // 读完后关
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("raat-sign: 签名服务对内容 %s（keypair %s）返回 notFound", hash[:8], keypair)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("raat-sign: 签名服务返回 %d", resp.StatusCode)
	}
	var out struct {
		ContentHashes map[string]map[string]string `json:"contentHashes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("raat-sign: 响应解析失败: %w", err)
	}
	sigs, ok := out.ContentHashes[hash]
	if !ok {
		return "", fmt.Errorf("raat-sign: 响应缺少 %s 的签名", hash[:8])
	}
	sig, ok := sigs[keypair]
	if !ok {
		return "", fmt.Errorf("raat-sign: 响应缺少 keypair %s 的签名", keypair)
	}

	p.mu.Lock()
	p.cache[key] = sig
	p.saveCache()
	p.mu.Unlock()
	return sig, nil
}

// LoadScript 下发 Lua DSP 脚本（B10）。
//
//	{request:"load_script", script, signature, keypair, module, name}
//
// 签名决策（对齐 RaatZonePlayer 的 _code_signing_keys 检查）：
// 设备 info 的 code_signing_keys 为空（本机补丁版 RAATServer 即此形态，
// RAAT__manager_add_code_signing_key 被 NOP 后无注册密钥）→ 无签名路径
// （不带 signature/keypair 字段）；非空且 keypair 在其中 → 走 raat-sign
// 签名。**不内置任何 Lua 脚本**（接口就绪，脚本内容属产品决策）。
func (s *Sink) LoadScript(ctx context.Context, script, keypair, module, name string) error {
	s.mu.Lock()
	ctrl := s.ctrl
	closed := s.closed
	s.mu.Unlock()
	if ctrl == nil || closed {
		return fmt.Errorf("raat: %s 会话未建立", s.id)
	}

	args := map[string]any{
		"request": "load_script",
		"script":  script,
	}
	if module != "" {
		args["module"] = module
	}
	if name != "" {
		args["name"] = name
	}

	// 签名决策：设备 code_signing_keys 非空且 keypair 在其中 → 走签名
	if len(s.deviceCodeSigningKeys) > 0 && contains(s.deviceCodeSigningKeys, keypair) {
		proxy := s.signProxy
		if proxy == nil {
			proxy = &SignProxy{}
		}
		sig, err := proxy.Sign(ctx, script, keypair)
		if err != nil {
			return fmt.Errorf("raat: load_script 签名失败: %w", err)
		}
		args["signature"] = sig
		args["keypair"] = keypair
	}

	return ctrl.RequestSimple(ctx, "load_script", args, nil)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

var _ = io.Discard
