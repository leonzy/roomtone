package discovery

import (
	"context"
	"errors"
	"net"
	"net/url"
	"sync"
	"testing"
	"time"

	"songloft/internal/player"
	"songloft/internal/player/sink/dlna"
	"songloft/internal/player/sink/raop"
)

type fakeRegistry struct {
	mu      sync.Mutex
	players map[string]player.Player
	saved   []player.Player
}

func newFakeRegistry(ids ...string) *fakeRegistry {
	r := &fakeRegistry{players: map[string]player.Player{}}
	for _, id := range ids {
		r.players[id] = player.Player{ID: id}
	}
	return r
}

func (r *fakeRegistry) GetPlayer(id string) (player.Player, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.players[id]
	if !ok {
		return player.Player{}, player.ErrPlayerNotFound
	}
	return p, nil
}

func (r *fakeRegistry) has(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.players[id]
	return ok
}

func (r *fakeRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.players)
}

func (r *fakeRegistry) RegisterPlayer(_ context.Context, p player.Player) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.players[p.ID] = p
	r.saved = append(r.saved, p)
	return nil
}

type fakeScanner struct {
	kind player.Kind
	list []Speaker
	err  error
	// calls 记录被调用次数，用来验证协议过滤。
	calls int
}

func (f *fakeScanner) Kind() player.Kind { return f.kind }

func (f *fakeScanner) Scan(context.Context) ([]Speaker, error) {
	f.calls++
	return f.list, f.err
}

func TestScanClassifiesDevices(t *testing.T) {
	reg := newFakeRegistry("raop:aa:bb:cc:dd:ee:ff")
	raopScan := &fakeScanner{kind: player.KindRAOP, list: []Speaker{
		{ID: "raop:aa:bb:cc:dd:ee:ff", Kind: player.KindRAOP, Name: "客厅", Address: "10.0.0.2:5000"},
		{ID: "raop:11:22:33:44:55:66", Kind: player.KindRAOP, Name: "书房", Address: "10.0.0.3:5000", Caps: player.Caps{NeedsPassword: true}},
	}}
	dlnaScan := &fakeScanner{kind: player.KindDLNA, list: []Speaker{
		{ID: "dlna:uuid-1", Kind: player.KindDLNA, Name: "厨房", Address: "http://10.0.0.4:1400/d.xml"},
		// 与 RAOP 扫描结果重复的 id（理论上不会发生，但去重必须成立）
		{ID: "dlna:uuid-1", Kind: player.KindDLNA, Name: "厨房-dup", Address: "http://10.0.0.9/d.xml"},
	}}
	d := New(Options{
		Registry:  reg,
		Scanners:  []Scanner{raopScan, dlnaScan},
		Protocols: []string{"raop", "dlna"},
		Enabled:   true,
	})
	res, err := d.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Speakers) != 3 {
		t.Fatalf("去重后应有 3 台，实际 %d: %+v", len(res.Speakers), res.Speakers)
	}
	if len(res.Added) != 2 {
		t.Errorf("新增应为 2 台（含密码设备），实际 %v", res.Added)
	}
	addedSet := map[string]bool{res.Added[0]: true, res.Added[1]: true}
	if !addedSet["dlna:uuid-1"] || !addedSet["raop:11:22:33:44:55:66"] {
		t.Errorf("新增应含 dlna:uuid-1 与密码设备，实际 %v", res.Added)
	}
	if len(res.Updated) != 1 || res.Updated[0] != "raop:aa:bb:cc:dd:ee:ff" {
		t.Errorf("已存在的设备应记为 updated，实际 %v", res.Updated)
	}
	// A4：需要密码的设备照常注册（Caps.NeedsPassword 照记，UI 可标注），
	// 不再进 Skipped；无密码起播时 Open 报明确的 401 错误。
	if len(res.Skipped) != 0 {
		t.Errorf("不应再跳过设备，实际 %+v", res.Skipped)
	}
	foundProtected := false
	for _, sp := range res.Speakers {
		if sp.ID == "raop:11:22:33:44:55:66" {
			foundProtected = true
			if !sp.Caps.NeedsPassword {
				t.Errorf("密码设备的 Caps.NeedsPassword 应照记")
			}
		}
	}
	if !foundProtected {
		t.Errorf("需要密码的设备应出现在 Speakers 中")
	}
	// 重复项保留第一条地址
	for _, sp := range res.Speakers {
		if sp.ID == "dlna:uuid-1" && sp.Address != "http://10.0.0.4:1400/d.xml" {
			t.Errorf("重复项应保留第一条，实际 %q", sp.Address)
		}
	}
	if !reg.has("raop:11:22:33:44:55:66") {
		t.Error("需要密码的设备应照常入库（A4：不再跳过）")
	}
	if res.ScannedAt.IsZero() {
		t.Error("ScannedAt 应填写")
	}
	last, at := d.Last()
	if len(last.Added) != 2 || at.IsZero() {
		t.Errorf("Last 应返回最近一次结果（含密码设备），实际 %+v", last)
	}
}

func TestScanCollectsScannerErrors(t *testing.T) {
	reg := newFakeRegistry()
	bad := &fakeScanner{kind: player.KindDLNA, err: errors.New("组播被防火墙拦了")}
	good := &fakeScanner{kind: player.KindRAOP, list: []Speaker{
		{ID: "raop:aa:bb:cc:dd:ee:ff", Kind: player.KindRAOP, Name: "客厅"},
	}}
	d := New(Options{Registry: reg, Scanners: []Scanner{bad, good}, Protocols: []string{"raop", "dlna"}})
	res, err := d.Scan(context.Background())
	if err != nil {
		t.Fatal("单协议失败不应让整次扫描失败")
	}
	if len(res.Errors) != 1 {
		t.Errorf("应记录 1 条协议错误，实际 %v", res.Errors)
	}
	if len(res.Added) != 1 {
		t.Errorf("另一个协议的结果应照常入库，实际 %v", res.Added)
	}
}

func TestScanHonorsProtocols(t *testing.T) {
	reg := newFakeRegistry()
	raopScan := &fakeScanner{kind: player.KindRAOP}
	dlnaScan := &fakeScanner{kind: player.KindDLNA}
	d := New(Options{Registry: reg, Scanners: []Scanner{raopScan, dlnaScan}, Protocols: []string{"raop"}})
	if _, err := d.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if raopScan.calls != 1 || dlnaScan.calls != 0 {
		t.Errorf("只应扫 raop：raop=%d dlna=%d", raopScan.calls, dlnaScan.calls)
	}

	// 手动扫描不受 enabled 影响，只受 protocols 限制
	d.ApplyConfig(false, []string{"dlna"}, 60*time.Second)
	if _, err := d.Scan(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dlnaScan.calls != 1 {
		t.Errorf("关闭自动扫描后手动扫描仍应可用，dlna calls=%d", dlnaScan.calls)
	}
}

func TestScanWithoutRegistry(t *testing.T) {
	d := New(Options{Scanners: []Scanner{&fakeScanner{kind: player.KindRAOP}}})
	res, err := d.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Added) != 0 {
		t.Errorf("没有 registry 时不应有新增，实际 %v", res.Added)
	}
}

func TestClampInterval(t *testing.T) {
	cases := map[time.Duration]time.Duration{
		0:                DefaultInterval,
		5 * time.Second:  MinInterval,
		30 * time.Second: 30 * time.Second,
		2 * time.Hour:    MaxInterval,
	}
	for in, want := range cases {
		if got := clampInterval(in); got != want {
			t.Errorf("clampInterval(%v) = %v，期望 %v", in, got, want)
		}
	}
}

func TestRAOPScannerMapsIDs(t *testing.T) {
	s := &RAOPScanner{Timeout: time.Second, Query: func(context.Context, string, time.Duration) ([]raop.Entry, error) {
		return []raop.Entry{{
			Name: "AABBCCDDEEFF@Living Room._raop._tcp.local.",
			Addr: net.ParseIP("10.0.0.5"),
			Port: 7000,
			TXT:  []string{"ch=2", "sr=44100"},
		}}, nil
	}}
	list, err := s.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("应得 1 台，实际 %d", len(list))
	}
	sp := list[0]
	if sp.ID != "raop:aa:bb:cc:dd:ee:ff" {
		t.Errorf("id 应为 raop:aa:bb:...，实际 %q", sp.ID)
	}
	if sp.Address != "10.0.0.5:7000" || sp.Kind != player.KindRAOP {
		t.Errorf("地址/类型不对: %+v", sp)
	}
	if sp.Caps.DeviceID != "aabbccddeeff" {
		t.Errorf("caps.DeviceID 应带上，实际 %q", sp.Caps.DeviceID)
	}
}

func TestDLNAScannerFetchesDescription(t *testing.T) {
	loc, _ := url.Parse("http://10.0.0.6:1400/desc.xml")
	scanner := &DLNAScanner{
		Timeout: 200 * time.Millisecond,
		SSDP: func(context.Context, dlna.SSDPOptions) ([]dlna.SSDPDevice, error) {
			return []dlna.SSDPDevice{
				{UUID: "uuid-6", Location: loc, ST: dlna.STMediaRenderer},
				// 同一设备重复回
				{UUID: "uuid-6", Location: loc, ST: dlna.STAll, USN: "uuid:uuid-6::" + dlna.STMediaRenderer},
				// MediaServer：不该被当成音箱
				{UUID: "uuid-7", Location: loc, ST: "urn:schemas-upnp-org:device:MediaServer:1"},
			}, nil
		},
		Fetch: func(context.Context, *dlna.Client, string) (*dlna.Device, error) {
			return &dlna.Device{
				FriendlyName: "厨房音箱",
				ModelName:    "Sonos One",
				UDN:          "uuid:uuid-6",
				Location:     loc,
				Services:     map[string]dlna.Service{dlna.SvcAVTransport: {Name: dlna.SvcAVTransport}},
			}, nil
		},
	}
	list, err := scanner.Scan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("应得 1 台（去重且过滤 MediaServer），实际 %d: %+v", len(list), list)
	}
	sp := list[0]
	if sp.ID != "dlna:uuid-6" || sp.Name != "厨房音箱" || sp.Model != "Sonos One" {
		t.Errorf("映射不对: %+v", sp)
	}
	if sp.Address != loc.String() {
		t.Errorf("address 应存 LOCATION，实际 %q", sp.Address)
	}
	if sp.Caps.SyncTier != player.SyncAligned {
		t.Errorf("DLNA 的同步等级应为 aligned，实际 %v", sp.Caps.SyncTier)
	}
}

func TestDLNAScannerAllFetchesFail(t *testing.T) {
	loc, _ := url.Parse("http://10.0.0.6:1400/desc.xml")
	scanner := &DLNAScanner{
		SSDP: func(context.Context, dlna.SSDPOptions) ([]dlna.SSDPDevice, error) {
			return []dlna.SSDPDevice{{UUID: "u", Location: loc, ST: dlna.STMediaRenderer}}, nil
		},
		Fetch: func(context.Context, *dlna.Client, string) (*dlna.Device, error) {
			return nil, errors.New("连不上")
		},
	}
	if _, err := scanner.Scan(context.Background()); err == nil {
		t.Error("全部描述都取不到时应报错，让用户知道是网络问题而不是没设备")
	}
}

func TestDiscoveryLoopScansOnStart(t *testing.T) {
	reg := newFakeRegistry()
	sc := &fakeScanner{kind: player.KindRAOP, list: []Speaker{
		{ID: "raop:aa:bb:cc:dd:ee:ff", Kind: player.KindRAOP, Name: "客厅", Address: "10.0.0.2:5000"},
	}}
	d := New(Options{
		Registry:  reg,
		Scanners:  []Scanner{sc},
		Protocols: []string{"raop"},
		Enabled:   true,
		Interval:  10 * time.Second,
	})
	d.Start()
	defer d.Stop()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if reg.has("raop:aa:bb:cc:dd:ee:ff") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("启动后应立刻扫一轮，而不是等第一个间隔")
}

func TestDiscoveryLoopStops(t *testing.T) {
	d := New(Options{
		Registry:  newFakeRegistry(),
		Scanners:  []Scanner{&fakeScanner{kind: player.KindRAOP}},
		Protocols: []string{"raop"},
		Enabled:   true,
		Interval:  10 * time.Second,
	})
	d.Start()
	done := make(chan struct{})
	go func() { d.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop 应在扫描结束后立即返回，不能等满一个间隔")
	}
}

func TestApplyConfigWakesLoop(t *testing.T) {
	reg := newFakeRegistry()
	sc := &fakeScanner{kind: player.KindDLNA, list: []Speaker{
		{ID: "dlna:u1", Kind: player.KindDLNA, Name: "厨房", Address: "http://10.0.0.4:1400/d.xml"},
	}}
	d := New(Options{
		Registry:  reg,
		Scanners:  []Scanner{sc},
		Protocols: []string{"dlna"},
		Enabled:   false,
		Interval:  time.Hour,
	})
	d.Start()
	defer d.Stop()
	// 关着的时候不扫
	time.Sleep(100 * time.Millisecond)
	if n := reg.count(); n != 0 {
		t.Fatalf("关闭状态下不应扫描，实际登记了 %d 台", n)
	}
	// 打开后立刻扫，不用等一小时
	d.ApplyConfig(true, []string{"dlna"}, time.Hour)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if reg.has("dlna:u1") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("ApplyConfig 打开后应立即扫描")
}
