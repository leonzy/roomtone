package handlers

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"

	"songloft/internal/models"
	"songloft/internal/services"
)

type MCPSettingsHandler struct {
	configs  *services.ConfigService
	current  func() models.MCPSettings
	apply    func(models.MCPSettings) error
	external bool
	basePath string
}

func NewMCPSettingsHandler(configs *services.ConfigService, current func() models.MCPSettings, apply func(models.MCPSettings) error, external bool, basePath string) *MCPSettingsHandler {
	return &MCPSettingsHandler{configs: configs, current: current, apply: apply, external: external, basePath: basePath}
}

type mcpSettingsResponse struct {
	Enabled              bool           `json:"enabled"`
	Token                string         `json:"token"`
	Scopes               []string       `json:"scopes"`
	ManagedByEnvironment bool           `json:"managed_by_environment"`
	Endpoint             string         `json:"endpoint"`
	ConfigExample        map[string]any `json:"config_example"`
}

func (h *MCPSettingsHandler) view() mcpSettingsResponse {
	settings := h.current()
	return mcpSettingsResponse{
		Enabled: settings.Enabled, Token: settings.Token, Scopes: settings.Scopes,
		ManagedByEnvironment: h.external, Endpoint: h.basePath + "/mcp",
		ConfigExample: map[string]any{"mcpServers": map[string]any{"roomtone": map[string]any{
			"url":     "http://<服务器地址>:58091" + h.basePath + "/mcp",
			"headers": map[string]string{"Authorization": "Bearer " + settings.Token},
		}}},
	}
}

// GetMCPSettings 读取 MCP 设置与 Agent 配置示例。
// @Summary 获取 MCP 设置
// @Description 返回启用状态、权限、Token、端点及可复制的 Agent 配置。仅认证管理员可读取；managed_by_environment=true 时由 Docker 环境变量管理，不能通过 PUT 修改。
// @Tags 设置
// @Produce json
// @Success 200 {object} mcpSettingsResponse "MCP 设置与配置示例"
// @Security BearerAuth
// @Router /settings/mcp [get]
func (h *MCPSettingsHandler) GetMCPSettings(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, h.view())
}

// UpdateMCPSettings 更新 MCP 设置。
// @Summary 更新 MCP 设置
// @Description 支持运行时启用/停用、修改 scopes 和重新生成 Token，无需重启。scopes 支持 read/playback/speakers/metadata/automation；空数组表示保留原值。regenerate_token=true 会使旧 Token 立即失效。
// @Tags 设置
// @Accept json
// @Produce json
// @Param request body mcpSettingsUpdateRequest true "MCP 更新内容"
// @Success 200 {object} mcpSettingsResponse "更新后的设置与配置示例"
// @Failure 400 {object} map[string]string "权限或配置无效"
// @Failure 409 {object} map[string]string "配置由环境变量管理"
// @Security BearerAuth
// @Router /settings/mcp [put]
func (h *MCPSettingsHandler) UpdateMCPSettings(w http.ResponseWriter, r *http.Request) {
	if h.external {
		respondError(w, http.StatusConflict, "MCP 由环境变量管理，请在部署配置中修改", nil)
		return
	}
	var req mcpSettingsUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	before := h.current()
	settings := before
	if req.Enabled != nil {
		settings.Enabled = *req.Enabled
	}
	if req.Scopes != nil {
		settings.Scopes = req.Scopes
	}
	if req.RegenerateToken || settings.Token == "" {
		token, err := newMCPToken()
		if err != nil {
			respondError(w, http.StatusInternalServerError, "生成 Token 失败", err)
			return
		}
		settings.Token = token
	}
	if err := h.apply(settings); err != nil {
		respondError(w, http.StatusBadRequest, "MCP 设置无效", err)
		return
	}
	if err := h.configs.SetJSON("mcp", settings); err != nil {
		_ = h.apply(before)
		respondError(w, http.StatusInternalServerError, "保存 MCP 设置失败", err)
		return
	}
	respondJSON(w, http.StatusOK, h.view())
}

type mcpSettingsUpdateRequest struct {
	Enabled         *bool    `json:"enabled,omitempty"`
	Scopes          []string `json:"scopes,omitempty"`
	RegenerateToken bool     `json:"regenerate_token,omitempty"`
}

func newMCPToken() (string, error) {
	data := make([]byte, 36)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return "rtmcp_" + base64.RawURLEncoding.EncodeToString(data), nil
}
