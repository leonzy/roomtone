package dlna

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"songloft/internal/player/sink"
)

const sampleDescXML = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
  <device>
    <deviceType>urn:schemas-upnp-org:device:MediaRenderer:1</deviceType>
    <friendlyName>客厅音箱</friendlyName>
    <modelName>Sonos Play:1</modelName>
    <UDN>uuid:RINCON_1234</UDN>
    <serviceList>
      <service>
        <serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>
        <controlURL>/MediaRenderer/AVTransport/Control</controlURL>
        <eventSubURL>/MediaRenderer/AVTransport/Event</eventSubURL>
      </service>
      <service>
        <serviceType>urn:schemas-upnp-org:service:RenderingControl:1</serviceType>
        <controlURL>http://192.0.2.9:1400/RenderingControl/Control</controlURL>
        <eventSubURL>/MediaRenderer/RenderingControl/Event</eventSubURL>
      </service>
    </serviceList>
  </device>
</root>`

func TestParseDescription(t *testing.T) {
	loc, _ := url.Parse("http://192.0.2.9:1400/desc.xml")
	dev, err := ParseDescription(loc, strings.NewReader(sampleDescXML))
	if err != nil {
		t.Fatal(err)
	}
	if dev.FriendlyName != "客厅音箱" || dev.UUID() != "RINCON_1234" {
		t.Fatalf("基础字段错误: %+v", dev)
	}
	av := dev.Services[SvcAVTransport]
	if av.ControlURL == nil || av.ControlURL.String() != "http://192.0.2.9:1400/MediaRenderer/AVTransport/Control" {
		t.Fatalf("相对 controlURL 未按 LOCATION 归一: %v", av.ControlURL)
	}
	rc := dev.Services[SvcRenderingControl]
	if rc.ControlURL.String() != "http://192.0.2.9:1400/RenderingControl/Control" {
		t.Fatalf("绝对 controlURL 被改写: %v", rc.ControlURL)
	}
}

func TestParseDescriptionMissingAVTransport(t *testing.T) {
	xml := `<root><device><friendlyName>x</friendlyName></device></root>`
	if _, err := ParseDescription(&url.URL{Scheme: "http", Host: "h"}, strings.NewReader(xml)); err == nil {
		t.Fatal("缺 AVTransport 应报错")
	}
}

func TestBuildMetadata(t *testing.T) {
	md := BuildMetadata(Item{Title: "A & B", Artist: "艺人", Album: "专辑", URL: "http://192.0.2.1:58091/stream/1.wav", Duration: "0:03:20"})
	for _, want := range []string{
		`<dc:title>A &amp; B</dc:title>`,
		`<dc:creator>艺人</dc:creator>`,
		`<upnp:album>专辑</upnp:album>`,
		`protocolInfo="http-get:*:audio/wav:*"`,
		`duration="0:03:20"`,
		`object.item.audioItem.musicTrack`,
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("DIDL 缺 %q:\n%s", want, md)
		}
	}
}

func TestBuildEnvelope(t *testing.T) {
	body := string(BuildEnvelope("urn:schemas-upnp-org:service:AVTransport:1", "Seek",
		[]Arg{{"InstanceID", "0"}, {"Unit", "REL_TIME"}, {"Target", "0:00:05"}}))
	for _, want := range []string{
		`<u:Seek xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">`,
		`<InstanceID>0</InstanceID>`,
		`<Unit>REL_TIME</Unit>`,
		`<Target>0:00:05</Target>`,
		`</u:Seek>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("信封缺 %q:\n%s", want, body)
		}
	}
}

func TestParseLastChange(t *testing.T) {
	body := []byte(`<e:propertyset xmlns:e="urn:schemas-upnp-org:event-1-0"><e:property>` +
		`<LastChange>&lt;Event xmlns="urn:schemas-upnp-org:metadata-1-0/AVT/"&gt;&lt;InstanceID val="0"&gt;` +
		`&lt;TransportState val="PLAYING"/&gt;&lt;RelativeTimePosition val="0:01:23"/&gt;` +
		`&lt;/InstanceID&gt;&lt;/Event&gt;</LastChange></e:property></e:propertyset>`)
	vars := ParseLastChange(body)
	if vars["TransportState"] != "PLAYING" || vars["RelativeTimePosition"] != "0:01:23" {
		t.Fatalf("LastChange 解析错误: %+v", vars)
	}
}

func TestFormatAndParseDuration(t *testing.T) {
	if got := formatDuration(83 * time.Second); got != "0:01:23" {
		t.Fatalf("格式化错误: %s", got)
	}
	if got := formatDuration(3725 * time.Second); got != "1:02:05" {
		t.Fatalf("格式化错误: %s", got)
	}
	if got := parseDuration("1:02:05"); got != 3725*time.Second {
		t.Fatalf("解析错误: %v", got)
	}
	if got := parseDuration("NOT_IMPLEMENTED"); got != 0 {
		t.Fatalf("NOT_IMPLEMENTED 应为 0: %v", got)
	}
}

// fakeRenderer 假 DLNA 设备。
type fakeRenderer struct {
	srv  *httptest.Server
	mu   sync.Mutex
	acts []string
	fail bool
}

func newFakeRenderer(t *testing.T) *fakeRenderer {
	t.Helper()
	f := &fakeRenderer{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.Header.Get("SOAPAction")
		action = strings.Trim(action, `"`)
		if i := strings.LastIndex(action, "#"); i >= 0 {
			action = action[i+1:]
		}
		f.mu.Lock()
		f.acts = append(f.acts, action)
		fail := f.fail
		f.mu.Unlock()

		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault><detail><UPnPError><errorCode>501</errorCode><errorDescription>Action Failed</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`)
			return
		}
		var extra string
		switch action {
		case "GetTransportInfo":
			extra = `<CurrentTransportState>PLAYING</CurrentTransportState>`
		case "GetPositionInfo":
			extra = `<RelTime>0:00:12</RelTime>`
		}
		fmt.Fprintf(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>`+
			`<u:%sResponse xmlns:u="urn:schemas-upnp-org:service:AVTransport:1">%s</u:%sResponse>`+
			`</s:Body></s:Envelope>`, action, extra, action)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRenderer) device(t *testing.T) *Device {
	t.Helper()
	loc, _ := url.Parse(f.srv.URL + "/desc.xml")
	dev, err := ParseDescription(loc, strings.NewReader(sampleDescXML))
	if err != nil {
		t.Fatal(err)
	}
	// 描述里的 host 指向假服务器。
	for name, svc := range dev.Services {
		u := *svc.ControlURL
		u.Scheme = loc.Scheme
		u.Host = loc.Host
		svc.ControlURL = &u
		dev.Services[name] = svc
	}
	return dev
}

func (f *fakeRenderer) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.acts...)
}

func TestSinkCallSequence(t *testing.T) {
	f := newFakeRenderer(t)
	dev := f.device(t)
	s := New("p1", dev, nil, Options{PollInterval: time.Hour})
	ctx := context.Background()
	if err := s.Open(ctx, sink.StreamPlan{
		StreamURL: "http://192.0.2.1:58091/stream/1.wav",
		Position:  0,
		Title:     "测试曲目",
		Format:    sink.Format{Codec: sink.CodecWAV, SampleRate: 44100, Channels: 2, Bits: 16},
	}); err != nil {
		t.Fatalf("Open 失败: %v", err)
	}
	if err := s.Start(time.Now()); err != nil {
		t.Fatalf("Start 失败: %v", err)
	}
	// 等异步 Play 落地。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		calls := f.calls()
		if len(calls) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := s.SetVolume(0.42); err != nil {
		t.Fatalf("SetVolume 失败: %v", err)
	}
	if err := s.Seek(44100); err != nil {
		t.Fatalf("Seek 失败: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop 失败: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	got := strings.Join(f.calls(), ",")
	want := "SetAVTransportURI,Play,SetVolume,Seek,Stop"
	if got != want {
		t.Fatalf("调用序列错误:\n got=%s\nwant=%s", got, want)
	}
}

func TestSinkSeekBeforePlayWhenPositionSet(t *testing.T) {
	f := newFakeRenderer(t)
	s := New("p2", f.device(t), nil, Options{PollInterval: time.Hour})
	if err := s.Open(context.Background(), sink.StreamPlan{
		StreamURL: "http://192.0.2.1/x.wav",
		Position:  90 * time.Second,
	}); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := strings.Join(f.calls(), ",")
	if got != "SetAVTransportURI,Seek" {
		t.Fatalf("带位置的 Open 应先 Seek: %s", got)
	}
}

func TestSinkErrorMapping(t *testing.T) {
	f := newFakeRenderer(t)
	f.fail = true
	s := New("p3", f.device(t), nil, Options{PollInterval: time.Hour})
	err := s.Open(context.Background(), sink.StreamPlan{StreamURL: "http://192.0.2.1/x.wav"})
	if err == nil {
		t.Fatal("设备 500 应报错")
	}
	var ae *ActionError
	if !asActionError(err, &ae) {
		t.Fatalf("错误类型不是 ActionError: %T %v", err, err)
	}
	if ae.Code != 501 || ae.Status != 500 {
		t.Fatalf("错误码映射不对: %+v", ae)
	}
	_ = s.Close()
}

func TestSinkPollUpdatesPosition(t *testing.T) {
	f := newFakeRenderer(t)
	s := New("p4", f.device(t), nil, Options{PollInterval: 30 * time.Millisecond})
	if err := s.Open(context.Background(), sink.StreamPlan{StreamURL: "http://192.0.2.1/x.wav"}); err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	deadline := time.Now().Add(2 * time.Second)
	var events []sink.Event
	for time.Now().Before(deadline) {
		select {
		case ev := <-s.Events():
			events = append(events, ev)
		case <-time.After(50 * time.Millisecond):
		}
		if s.Position() == 12*time.Second {
			break
		}
	}
	if s.Position() != 12*time.Second {
		t.Fatalf("轮询没有更新位置: %v", s.Position())
	}
	sawProgress := false
	for _, ev := range events {
		if ev.Type == sink.EventProgress && ev.Position == 12*time.Second {
			sawProgress = true
		}
	}
	if !sawProgress {
		t.Fatalf("没有上报 progress 事件: %+v", events)
	}
}

func TestPatchFor(t *testing.T) {
	if p := PatchFor("Sonos Play:1"); !p.TolerateEmptyMetadata || !p.SkipSetNext {
		t.Fatalf("Sonos 补丁未命中: %+v", p)
	}
	if p := PatchFor("Philips TV"); p.SeekUnit != "TRACK_NR" {
		t.Fatalf("Philips seek 单位错误: %+v", p)
	}
	if p := PatchFor("Unknown Device"); p.SeekUnit != "REL_TIME" {
		t.Fatalf("默认补丁错误: %+v", p)
	}
}

func asActionError(err error, target **ActionError) bool {
	for err != nil {
		if ae, ok := err.(*ActionError); ok {
			*target = ae
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
