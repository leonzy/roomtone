// Package raat 实现 RAAT（Roon Advanced Audio Transport）协议的发送端：
// SOOD 发现 → 控制通道（JSON 请求/响应）→ 音频数据通道（PCM/FLAC 帧）→
// 时钟同步。协议规格逆向自 Roon 2.74（RAAT-Protocol-Spec.md）。
package raat

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/ipv4"
)

// SOOD 常量（规格 §①）。
const (
	soodGroupAddr = "239.255.90.90:9003"
	soodVersion   = 0x02
	soodTypeQuery = 81 // 'Q'
	soodTypeResp  = 82 // 'R'

	// RaatServiceID RAAT 服务标识，响应的 service_id 必须等于它。
	RaatServiceID = "5e2042ad-9bc5-4508-be92-ff68f19bdc93"
)

// 错误（假 Transceiver 的非阻塞语义用）。
var (
	errTimeout = errors.New("raat/sood: 读超时")
	errAgain   = errors.New("raat/sood: 重试")
)

// Device 一台 SOOD 发现的 RAAT 设备（响应必填字段的精简形态）。
type Device struct {
	UniqueID string
	Model    string
	Vendor   string
	Version  string
	// RaatVersion RAAT 库版本（如 "1.1.48"）。
	RaatVersion string
	// ProtocolVersion 协议版本（int 字符串）。
	ProtocolVersion string
	// Name auto_name > output_name > unique_id。
	Name string
	// ConfigURL 设备配置页 URL（__SELF__ 占位符未替换，保留原样）。
	ConfigURL string
	// CodeSigningKeys 代码签名公钥名列表（逗号分隔；空 = 无签名路径）。
	CodeSigningKeys []string

	IP          net.IP
	ControlPort int
}

// Addr 控制通道地址 host:port。
func (d Device) Addr() string {
	if d.IP == nil {
		return ""
	}
	return net.JoinHostPort(d.IP.String(), fmt.Sprint(d.ControlPort))
}

// Transceiver SOOD 收发器（测试注入点）。
type Transceiver interface {
	// JoinAndListen 加入组播组并开始监听（单播回包与组播响应同 socket）。
	JoinAndListen() error
	// SendQuery 发送查询报文（组播 + 可选单播 trickle）。
	SendQuery(b []byte) error
	// Read 读一条报文（带截止时间；errTimeout/errAgain 表示非阻塞语义）。
	Read(p []byte) (int, *net.UDPAddr, error)
	// Deadline 设置下一次 Read 的截止时间。
	Deadline(t time.Time)
	Close() error
}

// buildQuery 构造 type=81 查询报文：KV{_tid: tid}。
func buildQuery(tid string) []byte {
	out := make([]byte, 0, 6+4+2+len(tid))
	out = append(out, 'S', 'O', 'O', 'D', soodVersion, soodTypeQuery)
	out = appendKV(out, "_tid", tid)
	return out
}

func appendKV(out []byte, k, v string) []byte {
	out = append(out, byte(len(k)))
	out = append(out, k...)
	var l [2]byte
	binary.BigEndian.PutUint16(l[:], uint16(len(v)))
	out = append(out, l[:]...)
	return append(out, v...)
}

// parseResponse 解析 type=82 响应。必填字段缺失或 service_id 不匹配
// 返回 false；KV 边界（截断/越界）返回 false 不 panic。
func parseResponse(data []byte) (Device, bool) {
	var d Device
	if len(data) < 6 || string(data[:4]) != "SOOD" {
		return d, false
	}
	if data[4] != soodVersion || data[5] != soodTypeResp {
		return d, false
	}
	kvs := map[string]string{}
	for i := 6; i < len(data); {
		if i+1 > len(data) {
			return d, false
		}
		klen := int(data[i])
		i++
		if i+klen > len(data) {
			return d, false
		}
		key := string(data[i : i+klen])
		i += klen
		if i+2 > len(data) {
			return d, false
		}
		vlen := int(binary.BigEndian.Uint16(data[i : i+2]))
		i += 2
		if i+vlen > len(data) {
			return d, false
		}
		kvs[key] = string(data[i : i+vlen])
		i += vlen
	}
	// 必填字段（规格 §①）：缺失或 service_id 不匹配则丢弃
	if kvs["service_id"] != RaatServiceID {
		return d, false
	}
	d.UniqueID = kvs["unique_id"]
	if d.UniqueID == "" {
		return d, false
	}
	d.Model = kvs["model"]
	d.Vendor = kvs["vendor"]
	d.Version = kvs["version"]
	d.RaatVersion = kvs["raat_version"]
	d.ProtocolVersion = kvs["protocol_version"]
	d.ControlPort = atoi(kvs["tcp_port"])
	if d.ControlPort == 0 {
		return d, false
	}
	// Name：auto_name > output_name > unique_id
	d.Name = kvs["auto_name"]
	if d.Name == "" {
		d.Name = kvs["output_name"]
	}
	if d.Name == "" {
		d.Name = d.UniqueID
	}
	d.ConfigURL = kvs["config_url"]
	if keys := kvs["code_signing_keys"]; keys != "" {
		d.CodeSigningKeys = strings.Split(keys, ",")
	}
	return d, true
}

func atoi(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// Discover 组播两轮查询，收集 type=82 响应（注入 transceiver 供测试）。
//
// 两轮查询间隔 timeout/2（对齐 mDNS 扫描节奏）；响应按 UniqueID 去重，
// 结果按 UniqueID 排序（稳定输出）。
func Discover(ctx context.Context, timeout time.Duration, xc Transceiver) ([]Device, error) {
	if xc == nil {
		xc = &udpTransceiver{}
	}
	if err := xc.JoinAndListen(); err != nil {
		return nil, fmt.Errorf("raat/sood: 加入组播组失败: %w", err)
	}
	defer xc.Close() //nolint:errcheck // 关闭失败无需处理

	seen := map[string]Device{}
	deadline := time.Now().Add(timeout)
	roundInterval := timeout / 2

	sendRound := func() error {
		var tid [8]byte
		rand.Read(tid[:]) //nolint:errcheck // crypto/rand 只在极罕见时失败
		return xc.SendQuery(buildQuery(fmt.Sprintf("%x", tid[:])))
	}

	if err := sendRound(); err != nil {
		return nil, fmt.Errorf("raat/sood: 发送查询失败: %w", err)
	}
	nextRound := time.Now().Add(roundInterval)
	buf := make([]byte, 1500)
	for {
		select {
		case <-ctx.Done():
			return sortDevices(seen), ctx.Err()
		default:
		}
		// 先查截止再发轮询：避免在 deadline 边缘多发一轮
		if time.Now().After(deadline) {
			return sortDevices(seen), nil
		}
		if time.Now().After(nextRound) {
			if err := sendRound(); err != nil {
				return nil, fmt.Errorf("raat/sood: 发送第二轮查询失败: %w", err)
			}
			nextRound = time.Now().Add(roundInterval)
		}
		xc.Deadline(time.Now().Add(minDuration(time.Until(nextRound), time.Until(deadline))))
		n, from, err := xc.Read(buf)
		if err != nil {
			if errors.Is(err, errTimeout) {
				continue
			}
			if errors.Is(err, errAgain) {
				continue
			}
			// 真实 socket 的超时也按轮询处理
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return nil, fmt.Errorf("raat/sood: 读取响应失败: %w", err)
		}
		if n == 0 {
			continue
		}
		dev, ok := parseResponse(buf[:n])
		if !ok {
			continue
		}
		if from != nil && from.IP != nil {
			dev.IP = from.IP
		}
		if _, dup := seen[dev.UniqueID]; !dup {
			seen[dev.UniqueID] = dev
		}
	}
}

func sortDevices(m map[string]Device) []Device {
	out := make([]Device, 0, len(m))
	for _, d := range m {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UniqueID < out[j].UniqueID })
	return out
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

// --- 真实 UDP 收发器 ---

// udpTransceiver 生产实现：单 socket 同时收组播响应与单播回包。
type udpTransceiver struct {
	conn *net.UDPConn
}

func (u *udpTransceiver) JoinAndListen() error {
	group, err := net.ResolveUDPAddr("udp4", soodGroupAddr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return err
	}
	// 组播加入（x/net/ipv4）：单 socket 同时收组播响应与单播回包。
	p := ipv4.NewPacketConn(conn)
	joined := false
	ifaces, err := net.Interfaces()
	if err == nil {
		for i := range ifaces {
			iface := ifaces[i]
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagMulticast == 0 {
				continue
			}
			if err := p.JoinGroup(&iface, group); err != nil {
				continue
			}
			joined = true
		}
	}
	if !joined {
		conn.Close() //nolint:errcheck // 关闭失败无需处理
		return errors.New("raat/sood: 没有任何接口加入组播组")
	}
	u.conn = conn
	return nil
}

func (u *udpTransceiver) SendQuery(b []byte) error {
	group, err := net.ResolveUDPAddr("udp4", soodGroupAddr)
	if err != nil {
		return err
	}
	_, err = u.conn.WriteToUDP(b, group)
	return err
}

func (u *udpTransceiver) Read(p []byte) (int, *net.UDPAddr, error) {
	return u.conn.ReadFromUDP(p)
}

func (u *udpTransceiver) Deadline(t time.Time) {
	_ = u.conn.SetReadDeadline(t) //nolint:errcheck // 超时设置失败按轮询处理
}

func (u *udpTransceiver) Close() error {
	return u.conn.Close()
}
