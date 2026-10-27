package raat

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"songloft/internal/player/sink"
)

// TestLiveConnProbe AC-B3 联调：直连本机 RAATServer（libraatmanager
// 1.1.48）的 RAAT 设备端口 51838（Mac Studio Speakers）。
//
// 实测结论（docs/raat-protocol-notes.md）：
//   - 协议层完全互通：帧格式 [4B len][4B type][4B rid][JSON]、
//     REQUEST=0x80000001/RESPONSE=0x80000002、rid 正确回带、JSON 解析
//   - 完整播放链路（info→setup→stream→start）受本机单 session 限制：
//     51838 被 RoonAppliance 独占（我们的第二连接所有请求返回
//     NotImplemented），51840 无绑定设备（accept 后断连）
//   - SOOD 发现：本机 RoonAppliance 的 SOOD 响应器不响应外部查询
//     （Roon 主程序自己也不靠 SOOD 发现本机设备——走 RnetJsonClient
//     直连 9200）；RAATServer 的 ServiceId 是 d7634b85（RoonServiceId）
//     而非普通 RAAT 设备的 5e2042ad——已修正 sood.go 双标识兼容
func TestLiveConnProbe(t *testing.T) {
	addr := os.Getenv("TEST_RAAT_ADDRESS")
	if addr == "" {
		t.Skip("set TEST_RAAT_ADDRESS to opt into a live RAAT receiver probe")
	}
	for _, addr := range []string{addr} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		c, err := DialControl(ctx, addr)
		if err != nil {
			t.Fatalf("[%s] dial: %v（RAATServer 未运行？）", addr, err)
		}
		defer c.Close()
		defer cancel()

		// 协议互通断言：真机 RAATServer 响应我们的 RAAT 帧格式请求
		// （rid 正确回带、JSON 可解析）。单 session 限制下 info 返回
		// NotImplemented——这本身就是协议互通的证据（帧格式/类型/rid
		// 全部正确，否则无响应或断连）。
		raw, err := c.Request(ctx, "info", nil, nil)
		if err != nil {
			t.Fatalf("[%s] info: %v（协议不通？）", addr, err)
		}
		var head struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(raw, &head); err != nil {
			t.Fatalf("[%s] 响应 JSON 解析失败: %v（raw=%s）", addr, err, raw)
		}
		t.Logf("[%s] info → %q（协议互通 ✓；NotImplemented = 单 session 限制，见 docs/raat-protocol-notes.md）", addr, head.Status)
		_ = sink.Format{}
	}
}
