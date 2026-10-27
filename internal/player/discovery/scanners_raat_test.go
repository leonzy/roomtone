package discovery

import (
	"context"
	"net"
	"testing"
	"time"

	"songloft/internal/player"
	"songloft/internal/player/sink"
	"songloft/internal/player/sink/raat"
)

// TestRAATScannerScan B6：SOOD 发现 + TCP info 探测 → Speaker。
func TestRAATScannerScan(t *testing.T) {
	// 假 info 探测端点（TCP，设备侧应答 info）
	infoL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer infoL.Close()
	go func() {
		for {
			conn, err := infoL.Accept()
			if err != nil {
				return
			}
			go handleFakeInfo(conn)
		}
	}()

	devs := []raat.Device{
		{UniqueID: "uid-1", Name: "客厅", Model: "ModelX", Vendor: "VendorY",
			RaatVersion: "1.1.48", ProtocolVersion: "1",
			IP: net.IPv4(127, 0, 0, 1), ControlPort: infoL.Addr().(*net.TCPAddr).Port},
		{UniqueID: "uid-2", Name: "书房", Model: "ModelZ", Vendor: "VendorY",
			RaatVersion: "1.1.48", ProtocolVersion: "1",
			IP: net.IPv4(127, 0, 0, 1), ControlPort: infoL.Addr().(*net.TCPAddr).Port},
	}
	s := &RAATScanner{
		Timeout: 100 * time.Millisecond,
		Discover: func(ctx context.Context, timeout time.Duration) ([]raat.Device, error) {
			return devs, nil
		},
		ProbeInfo: func(ctx context.Context, addr string, timeout time.Duration) (raat.Info, error) {
			return raat.Info{
				SupportedFormats:   "44100/16/2, 48000/24/2, 88200/24/2",
				VolumeSupported:    true,
				TransportSupported: true,
			}, nil
		},
	}
	list, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("应发现 2 台，实际 %d: %+v", len(list), list)
	}
	for _, sp := range list {
		if sp.Kind != player.KindRAAT {
			t.Fatalf("Kind = %q", sp.Kind)
		}
		if sp.ID != "raat:uid-1" && sp.ID != "raat:uid-2" {
			t.Fatalf("ID = %q，期望 raat:<unique_id>", sp.ID)
		}
		if sp.Address == "" {
			t.Fatal("Address 为空")
		}
		if !sp.Caps.SupportsVolume || !sp.Caps.SupportsSeek {
			t.Fatalf("Caps 不符: %+v", sp.Caps)
		}
		if sp.Caps.MaxSampleRate != 88200 || sp.Caps.BitDepth != 24 || sp.Caps.Channels != 2 {
			t.Fatalf("supported_formats 解析不符: %+v", sp.Caps)
		}
		if sp.Caps.SyncTier != sink.SyncExact {
			t.Fatalf("SyncTier = %v，期望 exact", sp.Caps.SyncTier)
		}
		if sp.Caps.LatencyMs != 2000 {
			t.Fatalf("LatencyMs = %d，期望 2000", sp.Caps.LatencyMs)
		}
	}
}

// info 探测失败（超时）的设备 → 跳过且不整体报错。
func TestRAATScannerProbeFailure(t *testing.T) {
	s := &RAATScanner{
		Timeout: 50 * time.Millisecond,
		Discover: func(ctx context.Context, timeout time.Duration) ([]raat.Device, error) {
			return []raat.Device{
				{UniqueID: "good", IP: net.IPv4(127, 0, 0, 1), ControlPort: 1},
				{UniqueID: "bad", IP: net.IPv4(127, 0, 0, 1), ControlPort: 2},
			}, nil
		},
		ProbeInfo: func(ctx context.Context, addr string, timeout time.Duration) (raat.Info, error) {
			if addr != "" && addr[len(addr)-1] == '1' { // ControlPort=1 → 成功
				return raat.Info{SupportedFormats: "44100/16/2"}, nil
			}
			return raat.Info{}, context.DeadlineExceeded
		},
	}
	list, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "raat:good" {
		t.Fatalf("探测失败应跳过不报错: %+v", list)
	}
}

// supported_formats 解析单测："44100/16/2|1, 2822400/1/2" →
// MaxSampleRate=2822400、BitDepth=16（DSD 位深 1 不抬 BitDepth）、Channels=2（取最大）。
func TestParseSupportedFormats(t *testing.T) {
	maxRate, bits, channels := raat.ParseSupportedFormats("44100/16/2|1, 2822400/1/2")
	if maxRate != 2822400 {
		t.Fatalf("MaxSampleRate = %d，期望 2822400", maxRate)
	}
	if bits != 16 {
		t.Fatalf("BitDepth = %d，期望 16（DSD 位深 1 不计入）", bits)
	}
	if channels != 2 {
		t.Fatalf("Channels = %d，期望 2", channels)
	}
	// 空串 → 零值
	if r, b, c := raat.ParseSupportedFormats(""); r != 0 || b != 0 || c != 0 {
		t.Fatalf("空串应返回零值: %d/%d/%d", r, b, c)
	}
}

// handleFakeInfo 设备侧 info 应答（只验证连接可建立即可，探测逻辑由
// ProbeInfo 注入替身；这里提供真实路径的最小应答）。
func handleFakeInfo(conn net.Conn) {
	defer conn.Close()
	// 读首个请求帧头（8B）+ body，回 info 响应
	head := make([]byte, 8)
	if _, err := readN(conn, head); err != nil {
		return
	}
	total := int(head[0])<<24 | int(head[1])<<16 | int(head[2])<<8 | int(head[3])
	if total > 8 {
		body := make([]byte, total-8)
		if _, err := readN(conn, body); err != nil {
			return
		}
	}
	resp := `{"status":"Success","output":{"supported_formats":"44100/16/2, 48000/24/2, 88200/24/2"},"volume":{"is_supported":true},"transport":{"is_supported":true}}`
	rid := []byte{0, 0, 0, 1}
	body := append(append([]byte{}, rid...), byte(0x01))
	body = append(body, resp...)
	frame := make([]byte, 8, 8+len(body))
	frame[4] = 0x80
	frame[5] = 0
	frame[6] = 0
	frame[7] = 2 // RESPONSE
	totalB := 8 + len(body)
	frame[0] = byte(totalB >> 24)
	frame[1] = byte(totalB >> 16)
	frame[2] = byte(totalB >> 8)
	frame[3] = byte(totalB)
	frame = append(frame, body...)
	conn.Write(frame) //nolint:errcheck // 测试
}

func readN(conn net.Conn, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := conn.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
