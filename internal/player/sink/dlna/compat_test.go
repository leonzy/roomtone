package dlna

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"songloft/internal/player/sink"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDescriptionURLBase(t *testing.T) {
	location, _ := url.Parse("http://old.invalid/device/desc.xml")
	x := strings.Replace(sampleDescXML, "<device>", "<URLBase>http://renderer.invalid:1234/base/</URLBase><device>", 1)
	x = strings.Replace(x, "/MediaRenderer/AVTransport/Control", "control", 1)
	d, err := ParseDescription(location, strings.NewReader(x))
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Services[SvcAVTransport].ControlURL.String(); got != "http://renderer.invalid:1234/base/control" {
		t.Fatal(got)
	}
	if got := d.Services[SvcRenderingControl].ControlURL.Host; got != "192.0.2.9:1400" {
		t.Fatal(got)
	}
}

func TestEmbeddedRendererKeepsServicesTogether(t *testing.T) {
	location, _ := url.Parse("http://renderer.invalid/root.xml")
	x := strings.Replace(sampleDescXML, "<device>", "<device><friendlyName>Root</friendlyName><deviceList><device>", 1)
	x = strings.Replace(x, "</root>", "</deviceList></device></root>", 1)
	d, err := ParseDescription(location, strings.NewReader(x))
	if err != nil {
		t.Fatal(err)
	}
	if d.FriendlyName != "客厅音箱" || d.UUID() != "RINCON_1234" || d.Services[SvcRenderingControl].ControlURL == nil {
		t.Fatalf("wrong embedded renderer: %+v", d)
	}
}

func TestLiveMetadataDoesNotAdvertiseFiniteDuration(t *testing.T) {
	features := ContentFeatures(false, true)
	md := BuildMetadata(Item{Live: true, Duration: "0:00:00", MimeType: "audio/mpeg", ProtocolInfo: "http-get:*:audio/mpeg:" + features})
	if strings.Contains(md, ` duration=`) || !strings.Contains(md, "audioBroadcast") || !strings.Contains(md, "DLNA.ORG_OP=00") {
		t.Fatal(md)
	}
}

func TestRendererFractionalAndUnavailablePosition(t *testing.T) {
	if got := parseDuration("1:02:03.250"); got != time.Hour+2*time.Minute+3250*time.Millisecond {
		t.Fatal(got)
	}
	for _, value := range []string{"NOT_IMPLEMENTED", "", "NaN", "-1", "1e30"} {
		if _, valid := parseDurationOK(value); valid {
			t.Fatalf("accepted %q", value)
		}
	}
	f := newFakeRenderer(t)
	s := New("notify", f.device(t), nil, Options{})
	defer s.Close()
	s.position = 20 * time.Second
	s.HandleNotify([]byte(`<propertyset><property><LastChange>&lt;Event&gt;&lt;InstanceID val="0"&gt;&lt;RelativeTimePosition val="NOT_IMPLEMENTED"/&gt;&lt;/InstanceID&gt;&lt;/Event&gt;</LastChange></property></propertyset>`))
	if s.Position() != 20*time.Second {
		t.Fatal("unavailable report reset position")
	}
}

func TestLastChangeOnlyInstanceZeroMaster(t *testing.T) {
	inner := `<Event><InstanceID val="0"><Volume channel="Master" val="15"/><Volume channel="LF" val="99"/><TransportState val="PLAYING"/></InstanceID><InstanceID val="1"><Volume channel="Master" val="88"/><TransportState val="STOPPED"/></InstanceID></Event>`
	vars := ParseLastChange([]byte(`<propertyset><property><LastChange>` + xmlEscape(inner) + `</LastChange></property></propertyset>`))
	if vars["Volume"] != "15" || vars["TransportState"] != "PLAYING" {
		t.Fatal(vars)
	}
}

func TestSubscriptionHonorsGrantedLeaseAndRenewsDuringTrack(t *testing.T) {
	var subscriptions, renewals atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "SUBSCRIBE" {
			if r.Header.Get("SID") != "" {
				renewals.Add(1)
			} else {
				subscriptions.Add(1)
			}
			w.Header().Set("SID", "uuid:short-lease")
			w.Header().Set("TIMEOUT", "Second-1")
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	f := newFakeRenderer(t)
	d := f.device(t)
	svc := d.Services[SvcAVTransport]
	svc.EventSubURL, _ = url.Parse(server.URL)
	d.Services[SvcAVTransport] = svc
	s := New("lease", d, nil, Options{CallbackBase: "http://callback.invalid", PollInterval: time.Hour})
	defer s.Close()
	s.trySubscribe()
	if s.sub == nil || s.sub.timeout != time.Second {
		t.Fatal("ignored granted lease")
	}
	s.startPoller()
	deadline := time.Now().Add(2 * time.Second)
	for renewals.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if renewals.Load() == 0 || subscriptions.Load() != 1 {
		t.Fatal("long track did not renew subscription")
	}
}

type serializedTransport struct{ active, max atomic.Int32 }

func (tr *serializedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	n := tr.active.Add(1)
	defer tr.active.Add(-1)
	for old := tr.max.Load(); n > old && !tr.max.CompareAndSwap(old, n); old = tr.max.Load() {
	}
	time.Sleep(5 * time.Millisecond)
	return http.DefaultTransport.RoundTrip(r)
}

func TestControlsAndPollingDoNotOverlapOnRenderer(t *testing.T) {
	f := newFakeRenderer(t)
	tr := &serializedTransport{}
	s := New("serial", f.device(t), &Client{HC: &http.Client{Transport: tr}}, Options{})
	defer s.Close()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				s.poll(context.Background())
			} else if err := s.SetVolume(float64(i) / 100); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if tr.max.Load() != 1 {
		t.Fatalf("concurrent renderer requests: %d", tr.max.Load())
	}
}

func TestPostPlaySeekReadinessDoesNotOverlapPolling(t *testing.T) {
	f := newFakeRenderer(t)
	d := f.device(t)
	d.ModelName = "S12"
	tr := &serializedTransport{}
	s := New("ready", d, &Client{HC: &http.Client{Transport: tr}}, Options{PollInterval: time.Millisecond})
	defer s.Close()
	if err := s.Open(context.Background(), sink.StreamPlan{StreamURL: "http://example.invalid/audio.flac", Position: 30 * time.Second}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(time.Now()); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-s.Events():
			if ev.Type == sink.EventError {
				t.Fatal(ev.Err)
			}
			if ev.Type == sink.EventPlaying {
				if tr.max.Load() != 1 {
					t.Fatalf("readiness bypassed serialized controls: %d", tr.max.Load())
				}
				return
			}
		case <-deadline:
			t.Fatal("seek did not start")
		}
	}
}

func TestNativeVolumeRangeAndUnsupportedMute(t *testing.T) {
	var desired string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, `<scpd><serviceStateTable><stateVariable><name>Volume</name><allowedValueRange><minimum>10</minimum><maximum>50</maximum><step>2</step></allowedValueRange></stateVariable></serviceStateTable></scpd>`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(r.Header.Get("SOAPAction"), "#SetMute") {
			w.WriteHeader(500)
			io.WriteString(w, `<Envelope><Body><Fault><detail><UPnPError><errorCode>401</errorCode></UPnPError></detail></Fault></Body></Envelope>`)
			return
		}
		if strings.Contains(r.Header.Get("SOAPAction"), "#SetVolume") {
			desired = string(body)
		}
		io.WriteString(w, `<Envelope><Body><SetVolumeResponse/></Body></Envelope>`)
	}))
	defer server.Close()
	u, _ := url.Parse(server.URL)
	d := &Device{Services: map[string]Service{SvcRenderingControl: {Name: SvcRenderingControl, Type: "urn:test", ControlURL: u, SCPDURL: u}}}
	LoadVolumeRange(context.Background(), NewClient(), d)
	s := New("volume", d, nil, Options{})
	defer s.Close()
	for _, tc := range []struct{ percent, want int }{{0, 10}, {25, 20}, {50, 30}, {100, 50}} {
		if err := s.SetVolume(float64(tc.percent) / 100); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(desired, fmt.Sprintf("<DesiredVolume>%d</DesiredVolume>", tc.want)) {
			t.Fatalf("%d did not map to %d", tc.percent, tc.want)
		}
	}
}
