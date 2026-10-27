package raat

import (
	"context"
	"encoding/binary"
	"net"
	"testing"
	"time"
)

// fakeClockServer 假时钟设备：回 "RAATCLKR" + rid + remote_time + output_delay。
// remote_time 按 fakeDriftNS/秒 相对本地漂移，模拟设备时钟偏差。
type fakeClockServer struct {
	conn *net.UDPConn
	// 设备时间 = 本地单调时间 + offsetNS + driftNS × 经过时间
	offsetNS int64
	driftNS  int64 // 每秒漂移（ns）

	queries chan []byte
}

func newFakeClockServer(t *testing.T, offsetNS, driftNS int64) *fakeClockServer {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	s := &fakeClockServer{conn: conn, offsetNS: offsetNS, driftNS: driftNS, queries: make(chan []byte, 16)}
	go s.serve()
	t.Cleanup(func() { conn.Close() })
	return s
}

func (s *fakeClockServer) addr() *net.UDPAddr { return s.conn.LocalAddr().(*net.UDPAddr) }

// monotonicNS 单调时钟（ns）——与 ClockSource 的 GetLocalTime 同基
// （clockEpoch 在 clock.go 定义）。
func monotonicNS() int64 {
	return int64(time.Since(clockEpoch)) // time.Duration 即 ns
}

func (s *fakeClockServer) deviceTimeNS() int64 {
	elapsed := time.Since(clockEpoch)
	return monotonicNS() + s.offsetNS + s.driftNS*int64(elapsed.Seconds())
}

func (s *fakeClockServer) serve() {
	buf := make([]byte, 1024)
	for {
		n, from, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 12 || string(buf[:8]) != "RAATCLKQ" {
			continue
		}
		rid := binary.BigEndian.Uint32(buf[8:12])
		select {
		case s.queries <- append([]byte(nil), buf[:n]...):
		default:
		}
		// 回 RAATCLKR + rid + remote_time + output_delay
		resp := make([]byte, 28)
		copy(resp[0:8], "RAATCLKR")
		binary.BigEndian.PutUint32(resp[8:12], rid)
		binary.BigEndian.PutUint64(resp[12:20], uint64(s.deviceTimeNS()))
		binary.BigEndian.PutUint64(resp[20:28], uint64(11025)) // output_delay
		s.conn.WriteToUDP(resp, from)                          //nolint:errcheck // 测试
	}
}

// Sync 后 GetTime 与 fake 设备时间差 < RTT/2 + 处理余量。
func TestClockSourceSync(t *testing.T) {
	srv := newFakeClockServer(t, 50*int64(time.Millisecond), 0) // 设备快 50ms

	cs := NewClockSource()
	defer cs.Close()

	start := time.Now()
	if err := cs.Sync(context.Background(), srv.addr().String()); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("sync 耗时 %v（应 3 次探测即完成）", took)
	}
	if !cs.OK() {
		t.Fatal("sync 后应 OK")
	}

	// GetTime ≈ 设备时间（±5ms 容差，本地回环 RTT≈0）
	devNS := srv.deviceTimeNS()
	gotNS := cs.GetTime()
	diff := gotNS - devNS
	if diff < 0 {
		diff = -diff
	}
	if diff > 5*int64(time.Millisecond) {
		t.Fatalf("GetTime 与设备时间差 %v（期望 <5ms）", time.Duration(diff))
	}

	// DeviceTime 正反换算一致
	wall := time.Now()
	dev := cs.DeviceTime(wall)
	back := cs.WallTime(dev)
	if back.Sub(wall) > 10*time.Millisecond || wall.Sub(back) > 10*time.Millisecond {
		t.Fatalf("正反换算不一致: %v vs %v", wall, back)
	}

	// OutputDelay 记录（fake 回 11025ns）
	if cs.OutputDelay() != 11025 {
		t.Fatalf("OutputDelay = %d，期望 11025", cs.OutputDelay())
	}

	// 探测包形态：RAATCLKQ + 4B rid（≥3 次查询）
	qCount := 0
	deadline := time.Now().Add(100 * time.Millisecond)
	for time.Now().Before(deadline) && qCount < 3 {
		select {
		case q := <-srv.queries:
			if string(q[:8]) != "RAATCLKQ" {
				t.Fatalf("查询包 magic = %q", string(q[:8]))
			}
			if len(q) != 12 {
				t.Fatalf("查询包长度 = %d，期望 12", len(q))
			}
			qCount++
		case <-time.After(50 * time.Millisecond):
		}
	}
	if qCount < 3 {
		t.Fatalf("只收到 %d 个查询（期望 ≥3）", qCount)
	}
}

// drift 随样本累积可观测（fake 端按固定 ppm 偏移）。
func TestClockSourceDrift(t *testing.T) {
	// 设备每秒漂移 2ms（2000ppm——夸张值便于观测）
	srv := newFakeClockServer(t, 0, 2*int64(time.Millisecond))

	cs := NewClockSource()
	defer cs.Close()

	if err := cs.Sync(context.Background(), srv.addr().String()); err != nil {
		t.Fatalf("sync#1: %v", err)
	}
	r1 := cs.Result()
	// 第二次 sync（间隔极短，drift 观测窗口小；只验证字段存在且无 NaN）
	if err := cs.Sync(context.Background(), srv.addr().String()); err != nil {
		t.Fatalf("sync#2: %v", err)
	}
	r2 := cs.Result()
	if !r2.IsSuccess || !r1.IsSuccess {
		t.Fatal("两次 sync 都应成功")
	}
	// 第二次的 OffsetDelta 累计进 CumulativeDelta
	if r2.CumulativeDelta == 0 && r1.CumulativeDelta != 0 {
		t.Fatalf("CumulativeDelta 应累计: r1=%d r2=%d", r1.CumulativeDelta, r2.CumulativeDelta)
	}
}

// 无响应 → 超时返回错误（不成功）。
func TestClockSourceTimeout(t *testing.T) {
	// 无服务的端口
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	deadAddr := conn.LocalAddr().String()
	conn.Close()

	cs := NewClockSource()
	defer cs.Close()
	start := time.Now()
	if err := cs.Sync(context.Background(), deadAddr); err == nil {
		t.Fatal("无响应应超时报错")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("超时耗时 %v（应 ~1s：200 迭代 × 5ms）", took)
	}
	if cs.OK() {
		t.Fatal("超时后不应 OK")
	}
}
