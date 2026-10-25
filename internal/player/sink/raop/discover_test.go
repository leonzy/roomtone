package raop

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSplitServiceName(t *testing.T) {
	cases := []struct {
		in       string
		wantID   string
		wantName string
	}{
		{"AABBCCDDEEFF@Living Room._raop._tcp.local.", "aabbccddeeff", "Living Room"},
		{"aa:bb:cc:dd:ee:ff@Kitchen._raop._tcp.local.", "aabbccddeeff", "Kitchen"},
		{"AA-BB-CC-DD-EE-FF@Office._airplay._tcp.local.", "aabbccddeeff", "Office"},
		{"AABBCCDDEEFF._raop._tcp.local.", "aabbccddeeff", ""},
		{"not-a-mac@X._raop._tcp.local.", "", "X"},
		{"", "", ""},
	}
	for _, c := range cases {
		id, name := SplitServiceName(c.in)
		if id != c.wantID || name != c.wantName {
			t.Errorf("SplitServiceName(%q) = (%q,%q)，期望 (%q,%q)", c.in, id, name, c.wantID, c.wantName)
		}
	}
}

func TestEntryToFoundParsesTXTAndKey(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pkB64 := base64.StdEncoding.EncodeToString(der)

	got, ok := EntryToFound(Entry{
		Name: "AABBCCDDEEFF@Living Room._raop._tcp.local.",
		Addr: net.ParseIP("192.168.1.20"),
		Port: 7000,
		TXT: []string{
			"ch=2", "cn=0,1", "et=0,4", "sr=44100", "ss=16",
			"pw=false", "tp=UDP", "md=AirPort10,115", "pk=" + pkB64,
		},
	})
	if !ok {
		t.Fatal("条目应当可用")
	}
	if got.DeviceID != "aabbccddeeff" || got.Name != "Living Room" {
		t.Errorf("设备身份不对: %+v", got)
	}
	if got.Address != "192.168.1.20:7000" {
		t.Errorf("地址不对: %q", got.Address)
	}
	if got.Model != "AirPort10,115" || got.Transport != "UDP" {
		t.Errorf("型号/传输不对: %q %q", got.Model, got.Transport)
	}
	if got.Caps.Channels != 2 || got.Caps.MaxSampleRate != 44100 || got.Caps.BitDepth != 16 {
		t.Errorf("能力解析不对: %+v", got.Caps)
	}
	if got.Caps.NeedsPassword {
		t.Error("pw=false 不该标记为需要密码")
	}
	if got.Caps.DeviceID != "aabbccddeeff" {
		t.Errorf("caps.DeviceID 应带上，RTSP 路径要用: %q", got.Caps.DeviceID)
	}
	if _, err := ParsePublicKey(got.Caps.PublicKey); err != nil {
		t.Fatalf("pk 应被归一成 PEM 且能解析: %v", err)
	}
}

func TestEntryToFoundRejectsIncomplete(t *testing.T) {
	if _, ok := EntryToFound(Entry{Name: "AABBCCDDEEFF@X._raop._tcp.local.", Port: 7000}); ok {
		t.Error("没有地址的条目应当丢弃")
	}
	if _, ok := EntryToFound(Entry{Name: "AABBCCDDEEFF@X._raop._tcp.local.", Addr: net.ParseIP("1.2.3.4")}); ok {
		t.Error("没有端口的条目应当丢弃")
	}
}

func TestDiscoverDedupesAndSorts(t *testing.T) {
	q := func(ctx context.Context, service string, timeout time.Duration) ([]Entry, error) {
		if service != ServiceName {
			t.Errorf("查询的服务应为 %q，实际 %q", ServiceName, service)
		}
		if timeout != 500*time.Millisecond {
			t.Errorf("超时应透传，实际 %v", timeout)
		}
		return []Entry{
			{Name: "BBBBBBBBBBBB@Zeta._raop._tcp.local.", Addr: net.ParseIP("10.0.0.2"), Port: 5000, TXT: []string{"ch=2"}},
			{Name: "AAAAAAAAAAAA@Alpha._raop._tcp.local.", Addr: net.ParseIP("10.0.0.1"), Port: 5000, TXT: []string{"ch=2"}},
			// 重复的 MAC：只保留第一条
			{Name: "AAAAAAAAAAAA@Alpha-dup._raop._tcp.local.", Addr: net.ParseIP("10.0.0.9"), Port: 5000, TXT: []string{"ch=2"}},
			// 缺端口：丢弃
			{Name: "CCCCCCCCCCCC@Broken._raop._tcp.local.", Addr: net.ParseIP("10.0.0.3")},
		}, nil
	}
	got, err := Discover(context.Background(), q, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("应得 2 台设备，实际 %d: %+v", len(got), got)
	}
	if got[0].Name != "Alpha" || got[1].Name != "Zeta" {
		t.Errorf("应按名称排序，实际 %q,%q", got[0].Name, got[1].Name)
	}
	if got[0].Address != "10.0.0.1:5000" {
		t.Errorf("重复项应保留第一条，实际 %q", got[0].Address)
	}
}

func TestDiscoverPropagatesQueryError(t *testing.T) {
	boom := errors.New("mdns 挂了")
	_, err := Discover(context.Background(), func(context.Context, string, time.Duration) ([]Entry, error) {
		return nil, boom
	}, time.Second)
	if !errors.Is(err, boom) {
		t.Fatalf("应透传查询错误，实际 %v", err)
	}
}

func TestNormalizePublicKey(t *testing.T) {
	if got := NormalizePublicKey(""); got != "" {
		t.Errorf("空值应返回空串，实际 %q", got)
	}
	if got := NormalizePublicKey("-----BEGIN PUBLIC KEY-----\nx\n-----END PUBLIC KEY-----"); !strings.Contains(got, "BEGIN PUBLIC KEY") {
		t.Errorf("PEM 应原样返回，实际 %q", got)
	}
	if got := NormalizePublicKey("not base64!!"); got != "" {
		t.Errorf("坏数据应返回空串，实际 %q", got)
	}
}

func TestParseTXTFieldsKeepsFirstDuplicate(t *testing.T) {
	got := ParseTXTFields([]string{"ch=2", "ch=6", "sr=44100", "novalue"})
	if got["ch"] != "2" {
		t.Errorf("重复键应取第一个，实际 %q", got["ch"])
	}
	if _, ok := got["novalue"]; ok {
		t.Error("没有 = 的字段应忽略")
	}
}
