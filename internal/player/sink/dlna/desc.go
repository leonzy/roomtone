package dlna

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// 服务类型短名，与 SOAPAction 的 urn 对应。
const (
	SvcAVTransport      = "AVTransport"
	SvcRenderingControl = "RenderingControl"
	SvcConnectionMgr    = "ConnectionManager"
)

// Service 一个 UPnP 服务端点。
type Service struct {
	Type        string // 完整 serviceType，如 urn:schemas-upnp-org:service:AVTransport:1
	Name        string // 短名 AVTransport
	ControlURL  *url.URL
	EventSubURL *url.URL
	SCPDURL     *url.URL
}

// Device 设备描述里我们关心的部分。
type Device struct {
	FriendlyName string
	ModelName    string
	UDN          string
	Location     *url.URL
	Services     map[string]Service
}

type descXML struct {
	Device struct {
		FriendlyName string `xml:"friendlyName"`
		ModelName    string `xml:"modelName"`
		UDN          string `xml:"UDN"`
		ServiceList  struct {
			Services []struct {
				ServiceType string `xml:"serviceType"`
				ServiceID   string `xml:"serviceId"`
				ControlURL  string `xml:"controlURL"`
				EventSubURL string `xml:"eventSubURL"`
				SCPDURL     string `xml:"SCPDURL"`
			} `xml:"service"`
		} `xml:"serviceList"`
		Devices []struct{} `xml:"deviceList>device"`
	} `xml:"device"`
}

// ParseDescription 解析 desc.xml。
//
// 相对 URL 必须按 LOCATION（不是按 SSDP 的 HOST）归一：很多设备把
// controlURL 写成 `/upnp/control/AVTransport1`，也有人写完整地址。
func ParseDescription(location *url.URL, r io.Reader) (*Device, error) {
	var d descXML
	if err := xml.NewDecoder(r).Decode(&d); err != nil {
		return nil, fmt.Errorf("dlna: 解析设备描述失败: %w", err)
	}
	dev := &Device{
		FriendlyName: strings.TrimSpace(d.Device.FriendlyName),
		ModelName:    strings.TrimSpace(d.Device.ModelName),
		UDN:          strings.TrimSpace(d.Device.UDN),
		Location:     location,
		Services:     map[string]Service{},
	}
	for _, s := range d.Device.ServiceList.Services {
		name := shortServiceName(s.ServiceType)
		if name == "" {
			continue
		}
		svc := Service{Type: s.ServiceType, Name: name}
		svc.ControlURL = resolveURL(location, s.ControlURL)
		svc.EventSubURL = resolveURL(location, s.EventSubURL)
		svc.SCPDURL = resolveURL(location, s.SCPDURL)
		if svc.ControlURL == nil {
			continue
		}
		dev.Services[name] = svc
	}
	if _, ok := dev.Services[SvcAVTransport]; !ok {
		return nil, errors.New("dlna: 设备没有 AVTransport 服务")
	}
	return dev, nil
}

func shortServiceName(t string) string {
	for _, n := range []string{SvcAVTransport, SvcRenderingControl, SvcConnectionMgr} {
		if strings.Contains(t, ":"+n+":") {
			return n
		}
	}
	return ""
}

func resolveURL(base *url.URL, ref string) *url.URL {
	ref = strings.TrimSpace(ref)
	if ref == "" || base == nil {
		return nil
	}
	u, err := url.Parse(ref)
	if err != nil {
		return nil
	}
	return base.ResolveReference(u)
}

// UDN 去掉 uuid: 前缀，返回纯 uuid。
func (d *Device) UUID() string {
	return strings.TrimPrefix(d.UDN, "uuid:")
}

// Fetch 从设备地址取 desc.xml 并解析。
//
// address 可以是完整 URL、host:port 或裸 IP；后两种按 DLNA 惯例补
// `http://<host>:<port>/desc.xml`（端口缺省 80/49152 由调用方给全）。
func Fetch(ctx context.Context, cl *Client, address string) (*Device, error) {
	loc, err := descURL(address)
	if err != nil {
		return nil, err
	}
	hc := cl.HC
	if hc == nil {
		hc = NewClient().HC
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loc.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dlna: 取设备描述失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dlna: 设备描述返回 %d", resp.StatusCode)
	}
	return ParseDescription(loc, io.LimitReader(resp.Body, 1<<20))
}

func descURL(address string) (*url.URL, error) {
	address = strings.TrimSpace(address)
	if address == "" {
		return nil, errors.New("dlna: 缺少设备地址")
	}
	if strings.HasPrefix(address, "http://") || strings.HasPrefix(address, "https://") {
		return url.Parse(address)
	}
	u := &url.URL{Scheme: "http", Host: address, Path: "/desc.xml"}
	return u, nil
}
