package raat

import (
	"context"
	"encoding/hex"
	"net"
	"testing"
	"time"
)

// golden：查询报文 = "SOOD" + ver 0x02 + type 81 + KV{_tid: tid}，
// KV = [1B keylen][key][2B BE vallen][value]。
func TestBuildQuery(t *testing.T) {
	q := buildQuery("abc123")
	want := "SOOD" +
		"\x02\x51" +
		"\x04_tid\x00\x06abc123"
	if string(q) != want {
		t.Fatalf("buildQuery 不符:\n got=%x\nwant=%x", q, want)
	}
	// 空 tid：KV 仍合法（vallen=0），报文更短
	qEmpty := buildQuery("")
	if string(qEmpty) != "SOOD\x02\x51\x04_tid\x00\x00" {
		t.Fatalf("空 tid 查询不符: %x", qEmpty)
	}
}

// golden：响应解析——必填字段、service_id 过滤、边界不 panic。
func TestParseResponse(t *testing.T) {
	// 构造一个合法 type=82 响应
	kv := func(k, v string) []byte {
		out := []byte{byte(len(k))}
		out = append(out, k...)
		out = append(out, byte(len(v)>>8), byte(len(v)))
		return append(out, v...)
	}
	build := func(fields ...string) []byte {
		out := []byte("SOOD\x02\x52")
		for i := 0; i+1 < len(fields); i += 2 {
			out = append(out, kv(fields[i], fields[i+1])...)
		}
		return out
	}

	// 合法响应
	resp := build(
		"unique_id", "dev-uid-1",
		"model", "ModelX",
		"vendor", "VendorY",
		"service_id", RaatServiceID,
		"tcp_port", "10001",
		"protocol_version", "1",
		"version", "1.0.0",
		"raat_version", "1.1.48",
		"auto_name", "客厅",
		"config_url", "http://__SELF__/cfg",
	)
	dev, ok := parseResponse(resp)
	if !ok {
		t.Fatal("合法响应应解析成功")
	}
	if dev.UniqueID != "dev-uid-1" || dev.Model != "ModelX" || dev.Vendor != "VendorY" {
		t.Fatalf("字段不符: %+v", dev)
	}
	if dev.ControlPort != 10001 || dev.ProtocolVersion != "1" || dev.RaatVersion != "1.1.48" {
		t.Fatalf("端口/版本不符: %+v", dev)
	}
	if dev.Name != "客厅" {
		t.Fatalf("Name 应取 auto_name: %q", dev.Name)
	}
	if dev.ConfigURL != "http://__SELF__/cfg" {
		t.Fatalf("ConfigURL = %q", dev.ConfigURL)
	}

	// service_id 不匹配 → 丢弃
	bad := build("unique_id", "x", "service_id", "not-raat", "tcp_port", "1")
	if _, ok := parseResponse(bad); ok {
		t.Fatal("service_id 不匹配应丢弃")
	}
	// RAATServer 的服务标识（d7634b85）也接受
	rs := build("unique_id", "rs", "service_id", RaatServerServiceID, "tcp_port", "1", "machine_name", "mac")
	if dev, ok := parseResponse(rs); !ok || dev.UniqueID != "rs" {
		t.Fatalf("RAATServer 服务标识应被接受: %+v", dev)
	}
	// 缺 service_id → 丢弃
	if _, ok := parseResponse(build("unique_id", "x")); ok {
		t.Fatal("缺 service_id 应丢弃")
	}
	// magic 错 / version!=2 / type!=82 → false
	if _, ok := parseResponse([]byte("XOOD\x02\x52")); ok {
		t.Fatal("magic 错应丢弃")
	}
	if _, ok := parseResponse([]byte("SOOD\x03\x52")); ok {
		t.Fatal("version!=2 应丢弃")
	}
	if _, ok := parseResponse([]byte("SOOD\x02\x51")); ok {
		t.Fatal("type!=82 应丢弃")
	}
	// KV 边界：key 截断 / value 长度越界 → false 不 panic
	if _, ok := parseResponse([]byte("SOOD\x02\x52\x04_ti")); ok {
		t.Fatal("key 截断应丢弃")
	}
	if _, ok := parseResponse(append([]byte("SOOD\x02\x52"), kv("_tid", "x")[:len(kv("_tid", "x"))-2]...)); ok {
		t.Fatal("value 截断应丢弃")
	}
	// 越界声明长度（vallen 声称 200 实际 3）→ false 不 panic
	if _, ok := parseResponse([]byte("SOOD\x02\x52\x04_tid\x00\xc8abc")); ok {
		t.Fatal("value 长度越界应丢弃")
	}
	// Name 回退：output_name > unique_id
	resp2 := build("unique_id", "uid", "output_name", "输出名", "service_id", RaatServiceID, "tcp_port", "1")
	if dev2, ok := parseResponse(resp2); !ok || dev2.Name != "输出名" {
		t.Fatalf("Name 应回退 output_name: %+v", dev2)
	}
	resp3 := build("unique_id", "uid", "service_id", RaatServiceID, "tcp_port", "1")
	if dev3, ok := parseResponse(resp3); !ok || dev3.Name != "uid" {
		t.Fatalf("Name 最终回退 unique_id: %+v", dev3)
	}
	_ = hex.EncodeToString
}

// --- 假 Transceiver ---

type fakeXC struct {
	// 每轮 Read 弹出一条响应；记录发送的查询
	sent     [][]byte
	deadline time.Time
	// 预置响应队列（每条带来源地址）
	responses []fakeResp
	closed    bool
}

type fakeResp struct {
	data []byte
	from *net.UDPAddr
}

func (f *fakeXC) JoinAndListen() error { return nil }

func (f *fakeXC) SendQuery(b []byte) error {
	f.sent = append(f.sent, append([]byte(nil), b...))
	return nil
}

func (f *fakeXC) Read(p []byte) (int, *net.UDPAddr, error) {
	if len(f.responses) == 0 {
		// 无响应：阻塞到 deadline（Discover 的超时路径）
		if !f.deadline.IsZero() && time.Now().After(f.deadline) {
			return 0, nil, errTimeout
		}
		time.Sleep(2 * time.Millisecond)
		return 0, nil, errAgain
	}
	r := f.responses[0]
	f.responses = f.responses[1:]
	n := copy(p, r.data)
	return n, r.from, nil
}

func (f *fakeXC) Deadline(t time.Time) { f.deadline = t }

func (f *fakeXC) Close() error {
	f.closed = true
	return nil
}

func makeResp(fields ...string) []byte {
	out := []byte("SOOD\x02\x52")
	for i := 0; i+1 < len(fields); i += 2 {
		k, v := fields[i], fields[i+1]
		out = append(out, byte(len(k)))
		out = append(out, k...)
		out = append(out, byte(len(v)>>8), byte(len(v)))
		out = append(out, v...)
	}
	return out
}

// Discover：两轮查询各发一个 type=81 报文；收到两个不同 unique_id 响应
// → 返回 2 个 Device 按 UniqueID 排序；超时无响应 → 空不报错。
func TestDiscover(t *testing.T) {
	xc := &fakeXC{
		responses: []fakeResp{
			{data: makeResp("unique_id", "uid-b", "service_id", RaatServiceID, "tcp_port", "10001", "raat_version", "1.1.48"),
				from: &net.UDPAddr{IP: net.IPv4(192, 0, 2, 20), Port: 9003}},
			{data: makeResp("unique_id", "uid-a", "service_id", RaatServiceID, "tcp_port", "10002", "auto_name", "A 设备"),
				from: &net.UDPAddr{IP: net.IPv4(192, 0, 2, 10), Port: 9003}},
			// 重复 uid（去重）
			{data: makeResp("unique_id", "uid-a", "service_id", RaatServiceID, "tcp_port", "10002"),
				from: &net.UDPAddr{IP: net.IPv4(192, 0, 2, 10), Port: 9003}},
		},
	}
	devs, err := Discover(context.Background(), 50*time.Millisecond, xc)
	if err != nil {
		t.Fatal(err)
	}
	if len(devs) != 2 {
		t.Fatalf("期望 2 台设备，实际 %d: %+v", len(devs), devs)
	}
	// 按 UniqueID 排序
	if devs[0].UniqueID != "uid-a" || devs[1].UniqueID != "uid-b" {
		t.Fatalf("排序不符: %+v", devs)
	}
	if devs[0].Name != "A 设备" || devs[0].ControlPort != 10002 {
		t.Fatalf("字段不符: %+v", devs[0])
	}
	if !devs[0].IP.Equal(net.IPv4(192, 0, 2, 10)) {
		t.Fatalf("IP 不符: %v", devs[0].IP)
	}
	// 两轮查询（间隔 timeout/2），各发一个 type=81
	if len(xc.sent) != 2 {
		t.Fatalf("期望 2 轮查询，实际 %d", len(xc.sent))
	}
	for i, q := range xc.sent {
		if q[0] != 'S' || q[4] != 0x02 || q[5] != 81 {
			t.Fatalf("第 %d 轮查询形态不符: %x", i+1, q)
		}
	}
	if !xc.closed {
		t.Fatal("Discover 结束后应关闭收发器")
	}

	// 超时无响应 → 空不报错
	xc2 := &fakeXC{}
	devs2, err := Discover(context.Background(), 20*time.Millisecond, xc2)
	if err != nil || len(devs2) != 0 {
		t.Fatalf("超时路径应返回空不报错: %v %+v", err, devs2)
	}
	if len(xc2.sent) != 2 {
		t.Fatalf("超时路径也应发两轮查询: %d", len(xc2.sent))
	}
}
