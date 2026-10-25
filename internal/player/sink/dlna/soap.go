package dlna

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ActionError 设备返回的 UPnP 错误。
type ActionError struct {
	Action string
	Status int
	Code   int    // UPnPError/errorCode，0 表示没解析到
	Desc   string // UPnPError/errorDescription
	Body   string
}

func (e *ActionError) Error() string {
	if e.Code != 0 {
		return fmt.Sprintf("dlna: %s 失败: HTTP %d, UPnP %d %s", e.Action, e.Status, e.Code, e.Desc)
	}
	return fmt.Sprintf("dlna: %s 失败: HTTP %d", e.Action, e.Status)
}

// Arg SOAP 参数。
type Arg struct{ Name, Value string }

// Client SOAP 客户端。
//
// 必须用裸 http.Client：DLNA 设备都在局域网，走系统的代理感知客户端
// 会被 HTTP_PROXY 劫持到代理上（见 docs/pitfalls.md#http-proxy）。
type Client struct {
	HC *http.Client
}

// NewClient 造一个不读环境代理的客户端。
func NewClient() *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.Proxy = nil
	return &Client{HC: &http.Client{Transport: tr, Timeout: 10 * time.Second}}
}

// Call 调一个动作，返回输出参数。
func (c *Client) Call(ctx context.Context, svc Service, action string, args ...Arg) (map[string]string, error) {
	if svc.ControlURL == nil {
		return nil, fmt.Errorf("dlna: 服务 %s 缺少 controlURL", svc.Name)
	}
	body := BuildEnvelope(svc.Type, action, args)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, svc.ControlURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", fmt.Sprintf(`"%s#%s"`, svc.Type, action))
	hc := c.HC
	if hc == nil {
		hc = NewClient().HC
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dlna: %s 请求失败: %w", action, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, newActionError(action, resp.StatusCode, raw)
	}
	out, err := ParseEnvelope(raw)
	if err != nil {
		return nil, fmt.Errorf("dlna: %s 响应无法解析: %w", action, err)
	}
	return out, nil
}

// BuildEnvelope 拼 SOAP 请求体（导出给 golden 测试）。
func BuildEnvelope(serviceType, action string, args []Arg) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?>`)
	b.WriteString(`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" `)
	b.WriteString(`s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>`)
	fmt.Fprintf(&b, `<u:%s xmlns:u="%s">`, action, serviceType)
	for _, a := range args {
		fmt.Fprintf(&b, "<%s>%s</%s>", a.Name, xmlEscape(a.Value), a.Name)
	}
	fmt.Fprintf(&b, `</u:%s></s:Body></s:Envelope>`, action)
	return []byte(b.String())
}

// ParseEnvelope 抽出 Body 下第一个元素的所有子元素文本。
func ParseEnvelope(raw []byte) (map[string]string, error) {
	dec := xml.NewDecoder(bytes.NewReader(raw))
	out := map[string]string{}
	depth := 0
	inBody := false
	bodyDepth := 0
	sawBody := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if t.Name.Local == "Body" {
				inBody = true
				sawBody = true
				bodyDepth = depth
				continue
			}
			if inBody && depth == bodyDepth+2 {
				var v string
				if err := dec.DecodeElement(&v, &t); err != nil {
					return nil, err
				}
				out[t.Name.Local] = v
				depth--
			}
		case xml.EndElement:
			if inBody && depth == bodyDepth {
				inBody = false
			}
			depth--
		}
	}
	if !sawBody {
		return nil, errors.New("响应里没有 SOAP Body")
	}
	// 无输出参数的动作（Play/Stop/Seek）Body 是空的，属正常。
	return out, nil
}

func newActionError(action string, status int, raw []byte) *ActionError {
	e := &ActionError{Action: action, Status: status, Body: string(raw)}
	var fault struct {
		Code int    `xml:"Body>Fault>detail>UPnPError>errorCode"`
		Desc string `xml:"Body>Fault>detail>UPnPError>errorDescription"`
	}
	if err := xml.Unmarshal(raw, &fault); err == nil {
		e.Code, e.Desc = fault.Code, fault.Desc
	}
	return e
}

// IsNotFound 设备不认识这个动作（部分老设备缺 SetNextAVTransportURI）。
func IsNotFound(err error) bool {
	var ae *ActionError
	if errors.As(err, &ae) {
		return ae.Code == 401 || ae.Status == http.StatusNotImplemented || ae.Status == http.StatusInternalServerError && ae.Code == 0
	}
	return false
}

func xmlEscape(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

// formatDuration 把时间格式化成 DLNA 的 H:MM:SS。
func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int64(d / time.Second)
	return fmt.Sprintf("%d:%02d:%02d", total/3600, (total%3600)/60, total%60)
}

// parseDuration 解析 H:MM:SS / MM:SS / 秒数。
func parseDuration(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" || s == "NOT_IMPLEMENTED" {
		return 0
	}
	parts := strings.Split(s, ":")
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			return 0
		}
		total = total*60 + n
	}
	return time.Duration(total) * time.Second
}
