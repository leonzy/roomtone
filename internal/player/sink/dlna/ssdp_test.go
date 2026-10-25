package dlna

import (
	"context"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBuildMSearchGolden(t *testing.T) {
	got := string(BuildMSearch("239.255.255.250:1900", STMediaRenderer, 2*time.Second))
	want := "M-SEARCH * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: 2\r\n" +
		"ST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"USER-AGENT: " + ssdpUserAgent + "\r\n\r\n"
	if got != want {
		t.Fatalf("M-SEARCH 报文不对:\n得到 %q\n期望 %q", got, want)
	}
	// MX 是秒数，亚秒的 MX 至少写 1，不能写 0（设备会当成立即回，丢包率高）
	if !strings.Contains(string(BuildMSearch("h:1", STAll, 200*time.Millisecond)), "MX: 1\r\n") {
		t.Error("MX 最小应为 1")
	}
}

func TestParseSSDPResponse(t *testing.T) {
	raw := []byte("HTTP/1.1 200 OK\r\n" +
		"CACHE-CONTROL: max-age=1800\r\n" +
		"EXT:\r\n" +
		"LOCATION: http://192.168.1.30:1400/xml/device_description.xml\r\n" +
		"SERVER: Linux/4.9 UPnP/1.0 Sonos/70.0\r\n" +
		"ST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"USN: uuid:RINCON_000E58A31D5C01400::urn:schemas-upnp-org:device:MediaRenderer:1\r\n\r\n")
	from := &net.UDPAddr{IP: net.ParseIP("192.168.1.30"), Port: 1900}
	got, err := ParseSSDPResponse(raw, from)
	if err != nil {
		t.Fatal(err)
	}
	if got.UUID != "RINCON_000E58A31D5C01400" {
		t.Errorf("UUID 解析不对: %q", got.UUID)
	}
	if got.Location.String() != "http://192.168.1.30:1400/xml/device_description.xml" {
		t.Errorf("LOCATION 解析不对: %q", got.Location)
	}
	if got.MaxAge != 1800*time.Second {
		t.Errorf("max-age 解析不对: %v", got.MaxAge)
	}
	if got.From != "192.168.1.30:1900" {
		t.Errorf("From 应记录对端: %q", got.From)
	}
	if !IsMediaRenderer(got.ST, got.USN) {
		t.Error("应识别为 MediaRenderer")
	}
}

func TestParseSSDPResponseErrors(t *testing.T) {
	if _, err := ParseSSDPResponse([]byte("HTTP/1.1 404 Not Found\r\nST: x\r\n\r\n"), nil); err == nil {
		t.Error("非 200 应报错")
	}
	if _, err := ParseSSDPResponse([]byte("HTTP/1.1 200 OK\r\nST: x\r\n\r\n"), nil); err == nil {
		t.Error("缺 LOCATION 应报错")
	}
	if _, err := ParseSSDPResponse([]byte("HTTP/1.1 200 OK\r\nLOCATION: not a url\r\n\r\n"), nil); err == nil {
		t.Error("坏 LOCATION 应报错")
	}
}

func TestIsMediaRenderer(t *testing.T) {
	if IsMediaRenderer(STAll, "uuid:x::urn:schemas-upnp-org:device:MediaServer:1") {
		t.Error("MediaServer 不算 renderer")
	}
	if !IsMediaRenderer(STAll, "uuid:x::urn:schemas-upnp-org:device:MediaRenderer:1") {
		t.Error("USN 里带 MediaRenderer 应识别")
	}
}

// TestDiscoverSSDPLoopback 用 127.0.0.1 上的假设备验证完整收发链路，
// 不发真实组播：CI 与无网卡的机器也能跑。
func TestDiscoverSSDPLoopback(t *testing.T) {
	dev, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Skipf("无法监听回环 UDP: %v", err)
	}
	defer dev.Close()

	var seen atomic.Int32
	gotST := make(chan string, 4)
	go func() {
		buf := make([]byte, 2048)
		for {
			_ = dev.SetReadDeadline(time.Now().Add(2 * time.Second))
			n, from, err := dev.ReadFromUDP(buf)
			if err != nil {
				return
			}
			req := string(buf[:n])
			seen.Add(1)
			for _, line := range strings.Split(req, "\r\n") {
				if strings.HasPrefix(line, "ST: ") {
					select {
					case gotST <- strings.TrimPrefix(line, "ST: "):
					default:
					}
				}
			}
			resp := "HTTP/1.1 200 OK\r\n" +
				"CACHE-CONTROL: max-age=60\r\n" +
				"LOCATION: http://127.0.0.1:1234/desc.xml\r\n" +
				"ST: " + STMediaRenderer + "\r\n" +
				"USN: uuid:fake-renderer::" + STMediaRenderer + "\r\n\r\n"
			// 故意回两次：发现逻辑必须去重
			_, _ = dev.WriteToUDP([]byte(resp), from)
			_, _ = dev.WriteToUDP([]byte(resp), from)
		}
	}()

	devices, err := DiscoverSSDP(context.Background(), SSDPOptions{
		Targets:    []string{dev.LocalAddr().String()},
		LocalAddrs: []net.IP{net.IPv4(127, 0, 0, 1)},
		Timeout:    600 * time.Millisecond,
		Retries:    1,
		MX:         time.Second,
	})
	if err != nil {
		t.Fatalf("扫描失败: %v", err)
	}
	if seen.Load() == 0 {
		t.Fatal("假设备没收到 M-SEARCH")
	}
	select {
	case st := <-gotST:
		if st != STMediaRenderer {
			t.Errorf("首个 ST 应为 MediaRenderer，实际 %q", st)
		}
	default:
		t.Error("没有解析出 ST")
	}
	if len(devices) != 1 {
		t.Fatalf("应得 1 台设备（去重后），实际 %d: %+v", len(devices), devices)
	}
	if devices[0].UUID != "fake-renderer" {
		t.Errorf("UUID 不对: %q", devices[0].UUID)
	}
	if devices[0].Location.String() != "http://127.0.0.1:1234/desc.xml" {
		t.Errorf("LOCATION 不对: %q", devices[0].Location)
	}
}

func TestDiscoverSSDPContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	_, err := DiscoverSSDP(ctx, SSDPOptions{
		Targets:    []string{"127.0.0.1:1"},
		LocalAddrs: []net.IP{net.IPv4(127, 0, 0, 1)},
		Timeout:    2 * time.Second,
		Retries:    1,
	})
	if err == nil {
		t.Error("ctx 已取消时应返回错误")
	}
	if time.Since(start) > time.Second {
		t.Errorf("取消应立即返回，实际耗时 %v", time.Since(start))
	}
}

func TestUsnUUID(t *testing.T) {
	cases := map[string]string{
		"uuid:abc::urn:x": "abc",
		"uuid:abc":        "abc",
		"":                "",
		"urn:x::urn:y":    "urn:x",
	}
	for in, want := range cases {
		if got := usnUUID(in); got != want {
			t.Errorf("usnUUID(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestParseMaxAge(t *testing.T) {
	if got := parseMaxAge("no-cache, max-age=120"); got != 120*time.Second {
		t.Errorf("max-age 解析不对: %v", got)
	}
	if got := parseMaxAge(""); got != 0 {
		t.Errorf("空值应为 0，实际 %v", got)
	}
}

var _ = url.Parse
