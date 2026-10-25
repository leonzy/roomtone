package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"songloft/internal/player"
	"songloft/internal/player/discovery"
	"songloft/internal/player/sink/dlna"
	"songloft/internal/player/sink/raop"
	"songloft/internal/services"
)

// DiscoveryRunner 设备发现能力。Bundle / 精简环境可以没有（接口为 nil）。
type DiscoveryRunner interface {
	Scan(ctx context.Context) (discovery.Result, error)
}

// SetDiscovery 注入发现器。
func (h *PlaybackHandler) SetDiscovery(d DiscoveryRunner) { h.discovery = d }

// SetAddressPolicy 注入"是否允许私网地址"的判断（来自 /settings/player-output）。
func (h *PlaybackHandler) SetAddressPolicy(allow func() bool) { h.allowPrivate = allow }

// SetOutputAllowlist 注入播放器出口白名单（来自 /settings/player-output-allowlist）。
//
// 与代理白名单是两条独立的链路（X-02）：那条管服务端出站 HTTP，
// 这条管往哪个网段推音频，配置键与判定都不共用。
func (h *PlaybackHandler) SetOutputAllowlist(f func() []*net.IPNet) { h.outputAllowlist = f }

// SetNetworkSinkPolicy 注入"能否使用网络音箱"的判断（X-04 Bundle 限制）。
//
// 返回 (是否允许, 不允许的原因)。iOS Bundle 里 Go 后端跑在 app 进程内，
// 推流目标与 App 生命周期纠缠，默认只留 local；桌面 Bundle 需要显式开启。
// 拒绝时把原因原样返回给前端，而不是静默失败——用户必须知道是"这个模式
// 不支持"还是"设备不在线"。
func (h *PlaybackHandler) SetNetworkSinkPolicy(f func() (bool, string)) { h.networkSinks = f }

func (h *PlaybackHandler) networkSinkCheck() error {
	if h.networkSinks == nil {
		return nil
	}
	if ok, reason := h.networkSinks(); !ok {
		return fmt.Errorf("%w: %s", errNetworkSinkDisabled, reason)
	}
	return nil
}

// errNetworkSinkDisabled 当前运行模式不允许网络音箱（映射成 403）。
var errNetworkSinkDisabled = errors.New("当前运行模式不支持网络音箱")

func (h *PlaybackHandler) outputAllowed() []*net.IPNet {
	if h.outputAllowlist == nil {
		return nil
	}
	return h.outputAllowlist()
}

func (h *PlaybackHandler) privateAllowed() bool {
	if h.allowPrivate == nil {
		return true
	}
	return h.allowPrivate()
}

// DiscoverPlayers 手动触发设备发现
// @Summary 手动扫描音箱
// @Description 立即执行一轮 mDNS（_raop._tcp）与 SSDP（MediaRenderer）扫描，把发现的设备登记成播放设备并返回结果。不受「自动发现」开关影响，只受 protocols 配置限制；已存在的设备会刷新地址与能力（added / updated 区分）。需要密码的音箱记入 skipped。
// @Tags 播放控制
// @Produce json
// @Success 200 {object} discovery.Result "扫描结果"
// @Failure 503 {object} map[string]string "设备发现不可用"
// @Security BearerAuth
// @Router /players/discover [post]
func (h *PlaybackHandler) DiscoverPlayers(w http.ResponseWriter, r *http.Request) {
	if h.discovery == nil {
		respondError(w, http.StatusServiceUnavailable, "设备发现不可用", nil)
		return
	}
	res, err := h.discovery.Scan(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, "设备扫描失败", err)
		return
	}
	respondJSON(w, http.StatusOK, res)
}

type addPlayerRequest struct {
	// Address 设备地址：DLNA 可填 IP / host:port / 完整 desc.xml 地址。
	Address string `json:"address"`
	// Kind raop 或 dlna。local 设备由客户端自己注册，不能手动添加。
	Kind string `json:"kind"`
	// Name 可选，留空用设备自己报的名字。
	Name string `json:"name"`
	// DeviceID RAOP 必填：AirPlay 的 MAC（RTSP 路径要用）。
	DeviceID string `json:"device_id"`
	// PublicKey RAOP 可选：设备公钥（PEM 或 TXT 里的 base64），填了才走加密。
	PublicKey string `json:"public_key"`
}

// AddPlayer 手动添加设备
// @Summary 手动添加音箱
// @Description 按地址直连一台音箱，用于发现不到的场景（Docker 桥接网络、跨网段、设备不广播）。DLNA 会先取 desc.xml 校验 AVTransport；RAOP 需要提供 device_id（MAC），否则无法拼出 RTSP 路径。地址在关闭「允许私网」时必须是公网地址。
// @Tags 播放控制
// @Accept json
// @Produce json
// @Param request body addPlayerRequest true "设备地址与类型"
// @Success 201 {object} player.Player "已添加的设备"
// @Failure 400 {object} map[string]string "参数错误或设备不可达"
// @Security BearerAuth
// @Router /players [post]
func (h *PlaybackHandler) AddPlayer(w http.ResponseWriter, r *http.Request) {
	var req addPlayerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "请求格式错误", err)
		return
	}
	kind := player.Kind(strings.ToLower(strings.TrimSpace(req.Kind)))
	if kind != player.KindRAOP && kind != player.KindDLNA {
		respondError(w, http.StatusBadRequest, "kind 只支持 raop / dlna", nil)
		return
	}
	if strings.TrimSpace(req.Address) == "" {
		respondError(w, http.StatusBadRequest, "address 不能为空", nil)
		return
	}
	if err := h.networkSinkCheck(); err != nil {
		respondError(w, http.StatusForbidden, err.Error(), nil)
		return
	}
	if err := h.checkAddress(r.Context(), kind, req.Address); err != nil {
		respondError(w, http.StatusBadRequest, err.Error(), nil)
		return
	}

	var p player.Player
	switch kind {
	case player.KindDLNA:
		cl := dlna.NewClient()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		// 裸 IP / host 补齐 scheme 与端口：dlna.Fetch 只认 host:port 或完整 URL，
		// 裸 IPv6 也靠 JoinHostPort 补上方括号。
		dev, err := dlna.Fetch(ctx, cl, withDefaultPort(req.Address, 80))
		if err != nil {
			respondError(w, http.StatusBadRequest, "设备不可达或不是 DLNA 音箱", err)
			return
		}
		id := dev.UUID()
		if id == "" {
			id = hostOfAddress(req.Address)
		}
		name := req.Name
		if name == "" {
			name = dev.FriendlyName
		}
		if name == "" {
			name = id
		}
		p = player.Player{
			ID:      "dlna:" + id,
			Kind:    player.KindDLNA,
			Name:    name,
			Model:   dev.ModelName,
			Address: dev.Location.String(),
			Caps:    dlna.CapsFor(dev),
		}
	case player.KindRAOP:
		deviceID := strings.TrimSpace(req.DeviceID)
		if deviceID == "" {
			respondError(w, http.StatusBadRequest, "RAOP 设备需要 device_id（AirPlay 的 MAC）；也可以用 POST /players/discover 自动发现", nil)
			return
		}
		caps := player.Caps{
			Codecs:         []player.Codec{player.CodecL16},
			MaxSampleRate:  44100,
			Channels:       2,
			BitDepth:       16,
			SupportsVolume: true,
			SupportsSeek:   true,
			LatencyMs:      2000,
			SyncTier:       player.SyncExact,
			DeviceID:       normalizeMAC(deviceID),
			PublicKey:      raop.NormalizePublicKey(req.PublicKey),
		}
		name := req.Name
		if name == "" {
			name = caps.DeviceID
		}
		p = player.Player{
			ID:      "raop:" + colonizeMAC(caps.DeviceID),
			Kind:    player.KindRAOP,
			Name:    name,
			Address: withDefaultPort(req.Address, 5000),
			Caps:    caps,
		}
	}

	if err := h.mgr.RegisterPlayer(r.Context(), p); err != nil {
		playbackFail(w, err)
		return
	}
	saved, err := h.mgr.GetPlayer(p.ID)
	if err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, saved)
}

// ReconnectPlayer 重连设备
// @Summary 重连离线音箱
// @Description 丢弃缓存的连接并重新握手，成功后把设备标回在线。用于音箱重启、换 IP 或路由器重连后 UI 的「重连」按钮；失败保持离线并返回 409。
// @Tags 播放控制
// @Produce json
// @Param id path string true "设备 ID"
// @Success 200 {object} player.Player "重连后的设备"
// @Failure 404 {object} map[string]string "设备不存在"
// @Failure 409 {object} map[string]string "设备仍不可达"
// @Security BearerAuth
// @Router /players/{id}/reconnect [post]
func (h *PlaybackHandler) ReconnectPlayer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	// 只对网络音箱做模式校验；本机设备在任何模式下都可用。
	if p, err := h.mgr.GetPlayer(id); err == nil && p.Kind != player.KindLocal {
		if err := h.networkSinkCheck(); err != nil {
			respondError(w, http.StatusForbidden, err.Error(), nil)
			return
		}
	}
	p, err := h.mgr.Reconnect(r.Context(), id)
	if err != nil {
		playbackFail(w, err)
		return
	}
	respondJSON(w, http.StatusOK, p)
}

// checkAddress 校验手动添加的地址：结构 + 私网策略。
func (h *PlaybackHandler) checkAddress(ctx context.Context, kind player.Kind, address string) error {
	host, _, err := splitHostPort(kind, address)
	if err != nil {
		return err
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("无法解析地址 %s", host)
	}
	if !h.privateAllowed() {
		for _, ip := range ips {
			if isPrivateIP(ip.IP) {
				return errors.New("当前设置不允许访问私网地址（/settings/player-output 的 allow_private_network）")
			}
		}
	}
	// 出口白名单：私网默认允许，配了白名单就只认白名单里的网段；
	// 公网地址一律拒绝——不往公网推音频，否则家里音箱就成了开放中继。
	allow := h.outputAllowed()
	for _, ip := range ips {
		if !services.IsPlayerOutputAllowed(ip.IP, allow) {
			return fmt.Errorf("播放器出口白名单不允许 %s（/settings/player-output-allowlist）", ip.IP)
		}
	}
	return nil
}

// splitHostPort 从 URL 或 host[:port] 里取主机与端口。
func splitHostPort(kind player.Kind, address string) (host, port string, err error) {
	address = strings.TrimSpace(address)
	if strings.Contains(address, "://") {
		u, perr := url.Parse(address)
		if perr != nil || u.Hostname() == "" {
			return "", "", fmt.Errorf("地址格式不正确: %s", address)
		}
		port = u.Port()
		if port == "" {
			port = "80"
		}
		return u.Hostname(), port, nil
	}
	host, port, err = net.SplitHostPort(address)
	if err == nil {
		if host == "" {
			return "", "", errors.New("地址缺少主机名")
		}
		return host, port, nil
	}
	// 只有主机：RAOP 默认 5000（AirPort Express / shairport-sync 的常用端口）
	if strings.Contains(address, ":") && net.ParseIP(address) == nil {
		return "", "", fmt.Errorf("地址格式不正确: %s", address)
	}
	if kind == player.KindRAOP {
		return address, "5000", nil
	}
	return address, "80", nil
}

func withDefaultPort(address string, port int) string {
	address = strings.TrimSpace(address)
	if strings.Contains(address, "://") {
		return address
	}
	if _, _, err := net.SplitHostPort(address); err == nil {
		return address
	}
	return net.JoinHostPort(address, strconv.Itoa(port))
}

func hostOfAddress(address string) string {
	host, _, err := splitHostPort(player.KindDLNA, address)
	if err != nil {
		return address
	}
	return host
}

func normalizeMAC(id string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(id) {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
			b.WriteRune(r)
		case r >= 'A' && r <= 'F':
			b.WriteRune(r - 'A' + 'a')
		}
	}
	return b.String()
}

func colonizeMAC(id string) string {
	if len(id) != 12 {
		return id
	}
	parts := make([]string, 0, 6)
	for i := 0; i < 12; i += 2 {
		parts = append(parts, id[i:i+2])
	}
	return strings.Join(parts, ":")
}

func isPrivateIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	return false
}
