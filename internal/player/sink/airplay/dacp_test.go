package airplay

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sync"
	"testing"
	"time"
)

// fakeControl 记录 OnControl 回调。
type fakeControl struct {
	mu   sync.Mutex
	cmds []struct {
		cmd   string
		value float64
	}
}

func (f *fakeControl) record(cmd string, value float64) {
	f.mu.Lock()
	f.cmds = append(f.cmds, struct {
		cmd   string
		value float64
	}{cmd, value})
	f.mu.Unlock()
}

func (f *fakeControl) snapshot() []struct {
	cmd   string
	value float64
} {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]struct {
		cmd   string
		value float64
	}(nil), f.cmds...)
}

func (f *fakeControl) waitLen(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.snapshot()) >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待超时：OnControl 只收到 %d 个（期望 %d）", len(f.snapshot()), n)
}

func get(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec // 测试内网地址
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	return resp.StatusCode
}

// TestDACPServerCommands 命令路由与 204/404 语义，逐字对齐 DACPServer.cs:122-139。
//
// 注意与计划文字的一处偏差（按反编译源为准）：/ctrl-int/1/ 前缀下的**任何**
// GET 都回 204 并回调 OnControl（音箱会轮询 playstatusupdate，未知命令回
// 404 会让轮询失败）；只有前缀之外的路径才 404。
func TestDACPServerCommands(t *testing.T) {
	fc := &fakeControl{}
	d := NewDACPServer(fc.record)
	if err := d.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer d.Close()
	base := fmt.Sprintf("http://127.0.0.1:%d", d.Port())

	// ① pause → 204 + ("pause", 0)
	if c := get(t, base+"/ctrl-int/1/pause"); c != 204 {
		t.Fatalf("pause 状态码 = %d，期望 204", c)
	}
	// ② setproperty?dmcp.device-volume=0.5 → ("setvolume", 0.5)
	if c := get(t, base+"/ctrl-int/1/setproperty?dmcp.device-volume=0.5"); c != 204 {
		t.Fatalf("setvolume 状态码 = %d，期望 204", c)
	}
	// ③ setproperty?dmcp.device-prevent-playback=1 → ("preventplayback", 0)
	if c := get(t, base+"/ctrl-int/1/setproperty?dmcp.device-prevent-playback=1"); c != 204 {
		t.Fatalf("preventplayback 状态码 = %d，期望 204", c)
	}
	// ④ 前缀下的未知命令 → 204 + 原样回调（对齐 DACPServer.cs）
	if c := get(t, base+"/ctrl-int/1/bogus"); c != 204 {
		t.Fatalf("bogus 状态码 = %d，期望 204", c)
	}
	// ⑤ 前缀之外的路径 → 404
	if c := get(t, base+"/other"); c != 404 {
		t.Fatalf("/other 状态码 = %d，期望 404", c)
	}
	// ⑥ setproperty 但既非音量也非 prevent-playback → 原样回调 setproperty
	if c := get(t, base+"/ctrl-int/1/setproperty?dmcp.unknown=1"); c != 204 {
		t.Fatalf("setproperty(unknown) 状态码 = %d，期望 204", c)
	}

	fc.waitLen(t, 5) // /other 是 404，不回调
	cmds := fc.snapshot()
	want := []struct {
		cmd   string
		value float64
	}{
		{"pause", 0},
		{"setvolume", 0.5},
		{"preventplayback", 0},
		{"bogus", 0},
		{"setproperty", 0},
	}
	for i, w := range want {
		if cmds[i].cmd != w.cmd || cmds[i].value != w.value {
			t.Fatalf("第 %d 个回调 = (%q,%v)，期望 (%q,%v)", i, cmds[i].cmd, cmds[i].value, w.cmd, w.value)
		}
	}
}

// TestDACPServerIdentity ID/ActiveRemote/实例名/TXT 对齐 DACPServer.cs 与
// DACPManager.cs 的约定：ID 为 16 位大写 hex，实例名 iTunes_Ctrl_<ID>，
// TXT 带 DACPId=<ID>。
func TestDACPServerIdentity(t *testing.T) {
	d := NewDACPServer(func(string, float64) {})
	if err := d.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer d.Close()

	if len(d.ID) != 16 {
		t.Fatalf("ID 长度 = %d，期望 16", len(d.ID))
	}
	if !regexp.MustCompile(`^[0-9A-F]{16}$`).MatchString(d.ID) {
		t.Fatalf("ID %q 不是 16 位大写 hex", d.ID)
	}
	if d.ActiveRemote == "" {
		t.Fatal("ActiveRemote 为空")
	}
	if got := d.Instance(); got != "iTunes_Ctrl_"+d.ID {
		t.Fatalf("实例名 = %q，期望 iTunes_Ctrl_%s", got, d.ID)
	}
	found := false
	for _, kv := range d.TXT() {
		if kv == "DACPId="+d.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("TXT %v 缺少 DACPId=%s", d.TXT(), d.ID)
	}
	// 两个实例 ID 不同（Guid 语义）。
	d2 := NewDACPServer(func(string, float64) {})
	if d2.ID == d.ID {
		t.Fatal("两个 DACPServer 的 ID 不应相同")
	}
}
