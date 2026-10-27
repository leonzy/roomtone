package raat

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// RaatClockSource 线格式（从 Roon.Broker.Core.dll 反编译提取，
// Roon.Broker.Transport.RaatClockSource.cs——注意：该类在 Broker.Core
// 而非计划假设的 Roon.Audio.dll，已修正提取路径）：
//
//	查询包（12B）："RAATCLKQ"(8B ASCII) + [4B BE rid]
//	响应包（28B）："RAATCLKR"(8B) + [4B BE rid] + [8B BE remote_time(ns)]
//	              + [8B BE output_delay(ns)]
//	错误包："RAATCLKE "(8B，注意尾部空格)
//
// Sync 语义：发查询（rid 递增），最多 200 次迭代（每次 sleep 5ms ≈ 1s
// 上限），收到 3 个响应即完成：每个响应 local_mid = xmit + (recv-xmit)/2、
// offset = remote_time - local_mid、rtt = (recv-xmit)/2；取 rtt 最小的
// offset 为 best。首次 sync 记 first_local_time，后续 sync 的
// offset 变化累计进 cumulative_delta（drift 观测）。
const (
	clockMagicQuery  = "RAATCLKQ"
	clockMagicResp   = "RAATCLKR"
	clockMagicError  = "RAATCLKE " // 尾部空格
	clockSyncIters   = 200
	clockSyncSleep   = 5 * time.Millisecond
	clockSyncResults = 3
)

// ClockSyncResult 一次 Sync 的结果（对齐 ClockSyncResult）。
type ClockSyncResult struct {
	IsSuccess       bool
	Offset          int64 // ns：设备时间 - 本地时间
	OffsetDelta     int64 // 本次 offset 相对上次的变化
	RoundTripTime   int64 // 最小 RTT（ns）
	CumulativeDelta int64 // 历次 OffsetDelta 累计
	AverageDrift    float64
	TotalTime       int64 // first → last local time（ns）
}

// clockEpoch 墙上时间 ↔ 设备时间换算的基准（包内共享）。
var clockEpoch = time.Now()

// ClockSource RAAT 时钟源：UDP 探测对（RAATCLKQ/RAATCLKR），
// 逐字移植 Roon.Broker.Transport.RaatClockSource。
type ClockSource struct {
	conn     *net.UDPConn
	timebase time.Time

	mu         sync.Mutex
	ridgen     int
	clockOff   int64  // ns
	outputDel  int64  // ns
	firstLocal *int64 // ns（首次 sync 的 local_mid）
	lastLocal  *int64 // ns
	cumDelta   int64
	result     ClockSyncResult
	ok         bool

	// pending 探测关联表（Sync 注册、readLoop 消费；持 mu）。
	pending map[int]*clockPending
}

// clockPending 一次探测的关联记录。
type clockPending struct {
	xmitTime int64
	onResp   func(recvTime, remoteTime int64)
}

// NewClockSource 创建时钟源（UDP 随机端口 + 读循环）。
func NewClockSource() *ClockSource {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if err != nil {
		return &ClockSource{timebase: time.Now(), pending: map[int]*clockPending{}}
	}
	cs := &ClockSource{conn: conn, timebase: time.Now(), pending: map[int]*clockPending{}}
	go cs.readLoop()
	return cs
}

// LocalPort 本地端口（0 表示无 socket）。
func (c *ClockSource) LocalPort() int {
	if c.conn == nil {
		return 0
	}
	return c.conn.LocalAddr().(*net.UDPAddr).Port
}

// Close 关闭。
func (c *ClockSource) Close() error {
	if c.conn != nil {
		return c.conn.Close()
	}
	return nil
}

// getLocalTime 单调时钟（ns）——对齐 GetLocalTime 的
// (Monotonic.Now - _timebase).Ticks * 100（TimeSpan.Ticks 是 100ns，
// ×100 得 ns；Go 的 time.Since 直接是 ns）。
func (c *ClockSource) getLocalTime() int64 {
	return int64(time.Since(c.timebase))
}

// GetTime 设备时间（本地 + offset）。
func (c *ClockSource) GetTime() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getLocalTime() + c.clockOff
}

// DeviceTime 墙上时间 → 设备时间。
func (c *ClockSource) DeviceTime(wall time.Time) int64 {
	c.mu.Lock()
	off := c.clockOff
	c.mu.Unlock()
	return int64(wall.Sub(clockEpoch)) + off
}

// WallTime 设备时间 → 墙上时间。
func (c *ClockSource) WallTime(devNS int64) time.Time {
	c.mu.Lock()
	off := c.clockOff
	c.mu.Unlock()
	return clockEpoch.Add(time.Duration(devNS - off))
}

// OutputDelay 设备回报的输出延迟（ns）。
func (c *ClockSource) OutputDelay() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.outputDel
}

// Result 最近一次 Sync 结果。
func (c *ClockSource) Result() ClockSyncResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.result
}

// OK 是否已成功同步。
func (c *ClockSource) OK() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ok
}

// Sync 同步：发探测对（最多 200 迭代 × 5ms），收 3 个响应即完成。
func (c *ClockSource) Sync(ctx context.Context, addr string) error {
	if c.conn == nil {
		return errors.New("raat/clock: 无 socket")
	}
	raddr, err := net.ResolveUDPAddr("udp4", addr)
	if err != nil {
		return fmt.Errorf("raat/clock: 地址解析失败: %w", err)
	}

	var (
		resultCount int
		offsetBest  int64
		rttBest     = int64(1<<62 - 1)
		done        bool
		rids        []int
	)

	// done/resultCount/offsetBest/rttBest 由回调在 c.mu 内写、本循环在
	// c.mu 内读（-race 干净）。
	for iter := 0; iter < clockSyncIters; iter++ {
		select {
		case <-ctx.Done():
			c.mu.Lock()
			for _, r := range rids {
				delete(c.pending, r)
			}
			c.mu.Unlock()
			return ctx.Err()
		default:
		}

		c.mu.Lock()
		if done {
			c.mu.Unlock()
			break
		}
		c.ridgen++
		rid := c.ridgen
		rids = append(rids, rid)
		xmit := c.getLocalTime()
		c.pending[rid] = &clockPending{
			xmitTime: xmit,
			onResp: func(recvTime, remoteTime int64) {
				// 在 c.mu 内执行（readLoop 持锁调用）
				resultCount++
				localMid := xmit + (recvTime-xmit)/2
				offset := remoteTime - localMid
				rtt := (recvTime - xmit) / 2
				if rtt < rttBest {
					offsetBest, rttBest = offset, rtt
				}
				if resultCount == clockSyncResults {
					done = true
					// 提交：更新 ClockSource 状态（已在 c.mu 内）
					prevOff := c.clockOff
					c.clockOff = offsetBest
					if c.firstLocal == nil {
						c.firstLocal = &localMid
						c.cumDelta = 0
					} else {
						c.cumDelta += c.clockOff - prevOff
					}
					c.lastLocal = &localMid
					c.result = ClockSyncResult{
						IsSuccess:       true,
						Offset:          c.clockOff,
						OffsetDelta:     c.clockOff - prevOff,
						RoundTripTime:   rttBest,
						CumulativeDelta: c.cumDelta,
						AverageDrift:    float64(c.cumDelta) / float64(localMid-*c.firstLocal),
						TotalTime:       localMid - *c.firstLocal,
					}
					c.ok = true
					for _, r := range rids {
						delete(c.pending, r)
					}
				}
			},
		}
		c.mu.Unlock()

		// 发查询：RAATCLKQ + rid
		q := make([]byte, 12)
		copy(q[0:8], clockMagicQuery)
		binary.BigEndian.PutUint32(q[8:12], uint32(rid))
		if _, err := c.conn.WriteToUDP(q, raddr); err != nil {
			c.mu.Lock()
			for _, r := range rids {
				delete(c.pending, r)
			}
			c.mu.Unlock()
			return fmt.Errorf("raat/clock: 发送失败: %w", err)
		}

		time.Sleep(clockSyncSleep)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if !done {
		for _, r := range rids {
			delete(c.pending, r)
		}
		c.ok = false
		return errors.New("raat/clock: 同步超时（无响应）")
	}
	return nil
}

// readLoop 读循环：RAATCLKR → 关联回调（持 mu）；RAATCLKE → 忽略。
func (c *ClockSource) readLoop() {
	buf := make([]byte, 1024)
	for {
		n, _, err := c.conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 28 || string(buf[:8]) != clockMagicResp {
			continue // RAATCLKE 等忽略
		}
		recvTime := c.getLocalTime()
		rid := int(binary.BigEndian.Uint32(buf[8:12]))
		remoteTime := int64(binary.BigEndian.Uint64(buf[12:20]))
		outputDelay := int64(binary.BigEndian.Uint64(buf[20:28]))
		c.mu.Lock()
		if outputDelay > 0 {
			c.outputDel = outputDelay
		}
		if p, ok := c.pending[rid]; ok {
			delete(c.pending, rid)
			p.onResp(recvTime, remoteTime)
		}
		c.mu.Unlock()
	}
}
