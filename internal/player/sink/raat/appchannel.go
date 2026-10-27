package raat

import (
	"context"
	"encoding/json"
	"fmt"
)

// appchannel.go B10：subscribe_app_channel / app_channel_send_message /
// force_exit。
//
//	conn.OnAppMessage func(channel string, msg json.RawMessage)
//	设备主动消息：type=2 通知 {"channel":..., "message":...} → OnAppMessage
//	SendAppMessage(ctx, channel, msg) → app_channel_send_message{channel, message}
//	ForceExit(ctx) → force_exit（显式方法，不进任何自动路径）
//
// v1 不接任何具体通道语义（对齐计划：接口就绪）。

// SendAppMessage 向设备 app channel 发消息。
func (s *Sink) SendAppMessage(ctx context.Context, channel string, msg any) error {
	s.mu.Lock()
	ctrl := s.ctrl
	s.mu.Unlock()
	if ctrl == nil {
		return fmt.Errorf("raat: %s 会话未建立", s.id)
	}
	return ctrl.RequestSimple(ctx, "app_channel_send_message", map[string]any{
		"channel": channel,
		"message": msg,
	}, nil)
}

// ForceExit 请求设备强制退出（RAATServer 管理用；显式方法，不进自动路径）。
func (s *Sink) ForceExit(ctx context.Context) error {
	s.mu.Lock()
	ctrl := s.ctrl
	s.mu.Unlock()
	if ctrl == nil {
		return fmt.Errorf("raat: %s 会话未建立", s.id)
	}
	return ctrl.RequestSimple(ctx, "force_exit", nil, nil)
}

// dispatchAppMessage 分发设备主动的 app channel 消息（conn.go 的
// OnServerRequest 路径调用：JSON 含 "channel" 键 → AppMessage）。
func dispatchAppMessage(s *Sink, raw json.RawMessage) bool {
	var note struct {
		Channel string `json:"channel"`
	}
	if err := json.Unmarshal(raw, &note); err != nil || note.Channel == "" {
		return false // 非 app channel 消息（transport/controls 通知）
	}
	s.mu.Lock()
	h := s.ctrl.OnAppMessage
	s.mu.Unlock()
	if h != nil {
		var msg json.RawMessage
		var wrapper struct {
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(raw, &wrapper); err == nil && len(wrapper.Message) > 0 {
			msg = wrapper.Message
		} else {
			msg = json.RawMessage(`{}`)
		}
		h(note.Channel, msg)
	}
	return true
}
