package airplay

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/mdns"
)

// DACPServer 对音箱的 DACP 反控端点（Roon.Audio.AirPlay.DACPServer 的 Go 对应物）。
//
// 音箱是 DACP 客户端：物理按键 / 遥控 → GET /ctrl-int/1/<cmd>；本服务端应答 204。
// 音箱通过 mDNS _dacp._tcp（实例名 iTunes_Ctrl_<DACPId>，对齐 DACPManager.cs:251）
// 发现反控端点，RTSP 请求头 DACP-ID / Active-Remote 指明身份。
//
// 语义逐字对齐 DACPServer.cs:122-139：/ctrl-int/1/ 前缀下的任何 GET 都回 204
// 并回调 onControl（音箱会轮询 playstatusupdate，未知命令也必须 204）；
// 前缀之外的路径 404；处理 panic 回 500。
type DACPServer struct {
	ID           string // 16 位大写 hex（对齐 DACPServer.cs:111）
	ActiveRemote string // 稳定 ID（对齐 DACPServer.cs:113-114，.NET UtcNow.Ticks）

	onControl func(cmd string, value float64)

	mu      sync.Mutex
	httpSrv *http.Server
	ln      net.Listener
	mdnsSrv *mdns.Server
}

// NewDACPServer 创建反控端点。onControl 为 nil 时命令只应答不回调。
func NewDACPServer(onControl func(cmd string, value float64)) *DACPServer {
	var idRaw [8]byte
	rand.Read(idRaw[:]) //nolint:errcheck // crypto/rand 只在极罕见时失败
	// 对齐 Guid.ToString("N").Substring(0,16).ToUpperInvariant()：16 位大写 hex。
	id := strings.ToUpper(hex.EncodeToString(idRaw[:]))[:16]
	return &DACPServer{
		ID:           id,
		ActiveRemote: strconv.FormatInt(time.Now().UTC().UnixNano()/100, 10),
		onControl:    onControl,
	}
}

// Start 监听临时端口并广播 _dacp._tcp。
//
// mDNS 广播失败只降级记日志（DACP 反控不可用，不影响播放主路径）——
// 多实例共用 5353 端口在部分环境下可能被拒。
func (d *DACPServer) Start() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ln != nil {
		return nil
	}
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		return fmt.Errorf("dacp: 监听失败: %w", err)
	}
	d.ln = ln
	d.httpSrv = &http.Server{Handler: http.HandlerFunc(d.serveHTTP)}
	go func() {
		if err := d.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			slog.Warn("DACP http server exited", "err", err)
		}
	}()

	port := ln.Addr().(*net.TCPAddr).Port
	svc, err := mdns.NewMDNSService(d.Instance(), "_dacp._tcp", "", "", port, nil, d.TXT())
	if err != nil {
		slog.Warn("DACP mDNS service 创建失败，反控发现不可用", "err", err)
		return nil
	}
	srv, err := mdns.NewServer(&mdns.Config{Zone: svc})
	if err != nil {
		slog.Warn("DACP mDNS server 启动失败，反控发现不可用", "err", err)
		return nil
	}
	d.mdnsSrv = srv
	return nil
}

// Instance mDNS 实例名（iTunes_Ctrl_<ID>，对齐 DACPManager.cs:251）。
func (d *DACPServer) Instance() string { return "iTunes_Ctrl_" + d.ID }

// TXT mDNS TXT 记录。
func (d *DACPServer) TXT() []string { return []string{"DACPId=" + d.ID} }

// Port 已监听的端口（Start 之后有效）。
func (d *DACPServer) Port() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ln == nil {
		return 0
	}
	return d.ln.Addr().(*net.TCPAddr).Port
}

// Close 停止 HTTP 与 mDNS。
func (d *DACPServer) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	var err error
	if d.httpSrv != nil {
		err = d.httpSrv.Close()
		d.httpSrv, d.ln = nil, nil
	}
	if d.mdnsSrv != nil {
		d.mdnsSrv.Shutdown() //nolint:errcheck // 关闭失败无需处理
		d.mdnsSrv = nil
	}
	return err
}

func (d *DACPServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			slog.Error("DACP handler panic", "recover", rec)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}()
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/ctrl-int/1/") {
		cmd := strings.TrimSpace(r.URL.Path[len("/ctrl-int/1/"):])
		value := 0.0
		if cmd == "setproperty" {
			q := r.URL.Query()
			if q.Has("dmcp.device-volume") {
				if v, err := strconv.ParseFloat(q.Get("dmcp.device-volume"), 64); err == nil {
					value = v
				}
				cmd = "setvolume"
			}
			if cmd == "setproperty" && q.Has("dmcp.device-prevent-playback") {
				if v, err := strconv.Atoi(q.Get("dmcp.device-prevent-playback")); err == nil && v == 1 {
					cmd = "preventplayback"
				}
			}
		}
		slog.Debug("DACP command", "cmd", cmd, "value", value)
		if d.onControl != nil {
			d.onControl(cmd, value)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}
