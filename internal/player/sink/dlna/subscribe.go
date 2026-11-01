package dlna

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Subscription GENA 事件订阅。
//
// 只负责 SUBSCRIBE / 续期 / 退订，NOTIFY 的接收由宿主决定（它得有个
// 局域网可达的 HTTP 入口）。订阅失败不是错误路径，调用方退回轮询。
type Subscription struct {
	cl      *Client
	svc     Service
	sid     string
	timeout time.Duration
	mu      sync.Mutex
	stopped bool
	renewAt time.Time
}

// Subscribe 发起订阅。callback 必须是设备可达的绝对 URL。
func Subscribe(ctx context.Context, cl *Client, svc Service, callback string, timeout time.Duration) (*Subscription, error) {
	if svc.EventSubURL == nil {
		return nil, fmt.Errorf("dlna: 服务 %s 没有 eventSubURL", svc.Name)
	}
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	req, err := http.NewRequestWithContext(ctx, "SUBSCRIBE", svc.EventSubURL.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("CALLBACK", "<"+callback+">")
	req.Header.Set("NT", "upnp:event")
	req.Header.Set("TIMEOUT", fmt.Sprintf("Second-%d", int(timeout/time.Second)))
	hc := cl.HC
	if hc == nil {
		hc = NewClient().HC
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dlna: SUBSCRIBE 失败: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dlna: SUBSCRIBE 返回 %d", resp.StatusCode)
	}
	sid := resp.Header.Get("SID")
	if sid == "" {
		return nil, fmt.Errorf("dlna: SUBSCRIBE 没有返回 SID")
	}
	timeout = subscriptionTimeout(resp.Header.Get("TIMEOUT"), timeout)
	return &Subscription{cl: cl, svc: svc, sid: sid, timeout: timeout, renewAt: time.Now().Add(timeout / 2)}, nil
}

func subscriptionTimeout(value string, fallback time.Duration) time.Duration {
	if strings.EqualFold(value, "Second-infinite") {
		return 24 * time.Hour
	}
	if len(value) > 7 && strings.EqualFold(value[:7], "Second-") {
		if seconds, err := strconv.Atoi(value[7:]); err == nil && seconds > 0 && seconds <= 86400 {
			return time.Duration(seconds) * time.Second
		}
	}
	return fallback
}

func (s *Subscription) RenewAfter() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := time.Until(s.renewAt)
	if d < 10*time.Millisecond {
		d = 10 * time.Millisecond
	}
	return d
}

// SID 订阅 id。
func (s *Subscription) SID() string { return s.sid }

// Renew 续期。设备允许在过期前续，间隔取 timeout 的一半。
func (s *Subscription) Renew(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped || s.svc.EventSubURL == nil {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, "SUBSCRIBE", s.svc.EventSubURL.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("SID", s.sid)
	req.Header.Set("TIMEOUT", fmt.Sprintf("Second-%d", int(s.timeout/time.Second)))
	hc := s.cl.HC
	if hc == nil {
		hc = NewClient().HC
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("dlna: 续订返回 %d", resp.StatusCode)
	}
	s.timeout = subscriptionTimeout(resp.Header.Get("TIMEOUT"), s.timeout)
	s.renewAt = time.Now().Add(s.timeout / 2)
	return nil
}

// Unsubscribe 退订，忽略错误。
func (s *Subscription) Unsubscribe(ctx context.Context) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.mu.Unlock()
	if s.svc.EventSubURL == nil {
		return
	}
	req, err := http.NewRequestWithContext(ctx, "UNSUBSCRIBE", s.svc.EventSubURL.String(), nil)
	if err != nil {
		return
	}
	req.Header.Set("SID", s.sid)
	hc := s.cl.HC
	if hc == nil {
		hc = NewClient().HC
	}
	if resp, err := hc.Do(req); err == nil {
		resp.Body.Close()
	}
}

// ParseLastChange 解析 GENA NOTIFY 里的 LastChange，返回变量名 → 值。
//
// 结构是三层嵌套 XML：propertyset → property → LastChange（转义后的
// Event → InstanceID → 各状态变量）。只取叶子节点的文本。
func ParseLastChange(body []byte) map[string]string {
	out := map[string]string{}
	var outer struct {
		Props []struct {
			LastChange string `xml:"LastChange"`
		} `xml:"property"`
	}
	if err := xml.Unmarshal(body, &outer); err != nil {
		return out
	}
	for _, p := range outer.Props {
		if strings.TrimSpace(p.LastChange) == "" {
			continue
		}
		// Event → InstanceID → 各状态变量，变量值在 val 属性里
		// （<TransportState val="PLAYING"/>），不是元素文本。
		dec := xml.NewDecoder(bytes.NewReader([]byte(p.LastChange)))
		instance := false
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			if end, ok := tok.(xml.EndElement); ok && end.Name.Local == "InstanceID" {
				instance = false
			}
			se, ok := tok.(xml.StartElement)
			if !ok {
				continue
			}
			if se.Name.Local == "InstanceID" {
				for _, a := range se.Attr {
					if a.Name.Local == "val" {
						instance = a.Value == "0"
					}
				}
				continue
			}
			if !instance {
				continue
			}
			master := true
			for _, a := range se.Attr {
				if a.Name.Local == "channel" && a.Value != "Master" {
					master = false
				}
			}
			if !master {
				continue
			}
			for _, a := range se.Attr {
				if a.Name.Local == "val" {
					out[se.Name.Local] = strings.TrimSpace(a.Value)
				}
			}
		}
	}
	return out
}
