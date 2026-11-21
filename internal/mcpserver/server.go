package mcpserver

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"songloft/internal/database"
	"songloft/internal/player"
	"songloft/internal/player/discovery"
	"songloft/internal/services"
	"songloft/internal/version"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	ScopeRead       = "read"
	ScopePlayback   = "playback"
	ScopeSpeakers   = "speakers"
	ScopeMetadata   = "metadata"
	ScopeAutomation = "automation"
)

type Config struct {
	Token  string
	Scopes []string
}

type DiscoveryRunner interface {
	Scan(context.Context) (discovery.Result, error)
}

type Deps struct {
	Songs        *services.SongService
	Playlists    *services.PlaylistService
	Players      *player.Manager
	Discovery    DiscoveryRunner
	SpeakerAdmin SpeakerAdmin
	Alarms       *database.AlarmRepository
	TriggerAlarm func(context.Context, string) error
	Configs      *services.ConfigService
	History      *services.ListeningHistoryService
	Recommend    *services.RecommendationService
	Metadata     *services.MetadataUpdateService
}

type Server struct {
	deps    Deps
	token   string
	scopes  map[string]bool
	mcp     *sdkmcp.Server
	handler http.Handler
}

func New(cfg Config, deps Deps) (*Server, error) {
	if cfg.Token == "" {
		return nil, errors.New("MCP_TOKEN 不能为空")
	}
	scopes := make(map[string]bool)
	for _, scope := range cfg.Scopes {
		scope = strings.TrimSpace(scope)
		switch scope {
		case ScopeRead, ScopePlayback, ScopeSpeakers, ScopeMetadata, ScopeAutomation:
			scopes[scope] = true
		case "":
		default:
			return nil, fmt.Errorf("未知 MCP scope %q", scope)
		}
	}
	if len(scopes) == 0 {
		return nil, errors.New("MCP_SCOPES 至少需要一个权限")
	}
	s := &Server{deps: deps, token: cfg.Token, scopes: scopes}
	s.mcp = sdkmcp.NewServer(&sdkmcp.Implementation{
		Name: "roomtone", Version: version.GetVersion(),
	}, &sdkmcp.ServerOptions{Instructions: "控制 Roomtone 曲库、播放和音箱。自然语言指代由 Agent 解析为明确 ID。搜索或读取状态后再操作；推荐不会自动播放；修改工具需要对应 scope。"})
	s.registerLibraryTools()
	s.registerPlaybackTools()
	s.registerSpeakerTools()
	s.registerZoneTools()
	s.registerPartyAutomationTools()
	s.registerSettingsTools()
	s.registerHistoryTools()
	s.handler = sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return s.mcp }, nil)
	return s, nil
}

// ServeHTTP 处理 MCP Streamable HTTP 请求。
// @Summary MCP Agent 控制入口
// @Description 使用 MCP Streamable HTTP 提供曲库、播放、音箱、分组、自动化、历史、推荐与元数据工具。协议请求和响应 schema 由 MCP SDK 动态协商，不由 OpenAPI 静态描述。需要独立 MCP Bearer Token，权限由 MCP_SCOPES 控制。
// @Tags 系统管理
// @Produce json
// @Success 200 {object} map[string]any "MCP JSON-RPC 响应"
// @Failure 401 {object} map[string]string "Token 缺失或无效"
// @Failure 403 {object} map[string]string "跨源请求被拒绝"
// @Security BearerAuth
// @Router /mcp [post]
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin MCP request rejected", http.StatusForbidden)
		return
	}
	auth := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
		w.Header().Set("WWW-Authenticate", `Bearer realm="roomtone-mcp"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	s.handler.ServeHTTP(w, r.WithContext(s.withScopes(r.Context())))
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

type scopeKey struct{}

func (s *Server) withScopes(ctx context.Context) context.Context {
	return context.WithValue(ctx, scopeKey{}, s.scopes)
}

func scopesFrom(ctx context.Context) map[string]bool {
	scopes, _ := ctx.Value(scopeKey{}).(map[string]bool)
	return scopes
}

func (s *Server) require(ctx context.Context, scope string) error {
	if scopesFrom(ctx)[scope] {
		return nil
	}
	return &ToolError{Code: "FORBIDDEN", Message: "当前 MCP Token 缺少 " + scope + " 权限"}
}

type ToolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *ToolError) Error() string { return e.Message }

func invalidInput(format string, args ...any) error {
	return &ToolError{Code: "INVALID_ARGUMENT", Message: fmt.Sprintf(format, args...)}
}

func domainError(err error) error {
	if err == nil {
		return nil
	}
	var toolErr *ToolError
	if errors.As(err, &toolErr) {
		return err
	}
	code := "INTERNAL_ERROR"
	switch {
	case errors.Is(err, database.ErrNotFound), errors.Is(err, player.ErrPlayerNotFound):
		code = "NOT_FOUND"
	case errors.Is(err, player.ErrGroupNotFound):
		code = "ZONE_NOT_FOUND"
	case errors.Is(err, player.ErrPartyNotFound):
		code = "PARTY_NOT_FOUND"
	case errors.Is(err, player.ErrPlayerOffline):
		code = "SPEAKER_OFFLINE"
	case errors.Is(err, player.ErrGroupInParty):
		code = "GROUP_IN_PARTY"
	case errors.Is(err, player.ErrPartyDisabled):
		code = "PARTY_DISABLED"
	case errors.Is(err, player.ErrUnsupported):
		code = "UNSUPPORTED_OPERATION"
	case errors.Is(err, player.ErrInvalidArgument), errors.Is(err, player.ErrInvalidName), errors.Is(err, player.ErrInvalidIcon), errors.Is(err, player.ErrBadMemberOrder):
		code = "INVALID_ARGUMENT"
	}
	return &ToolError{Code: code, Message: err.Error()}
}

func toolFailure(err error) *sdkmcp.CallToolResult {
	err = domainError(err)
	payload, _ := json.Marshal(map[string]any{"error": err})
	return &sdkmcp.CallToolResult{
		IsError: true,
		Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: string(payload)}},
	}
}

func registerTool[In any](s *Server, scope, name, description string, readOnly bool, fn func(context.Context, In) (any, error)) {
	annotations := &sdkmcp.ToolAnnotations{ReadOnlyHint: readOnly, IdempotentHint: readOnly}
	sdkmcp.AddTool(s.mcp, &sdkmcp.Tool{Name: name, Description: description, Annotations: annotations},
		func(ctx context.Context, _ *sdkmcp.CallToolRequest, in In) (*sdkmcp.CallToolResult, any, error) {
			if err := s.require(ctx, scope); err != nil {
				return toolFailure(err), nil, nil
			}
			out, err := fn(ctx, in)
			if !readOnly {
				slog.Info("MCP mutation", "tool", name, "success", err == nil)
			}
			if err != nil {
				return toolFailure(err), nil, nil
			}
			return nil, out, nil
		})
}
