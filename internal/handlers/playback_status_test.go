package handlers

import (
	"fmt"
	"net/http"
	"testing"

	"songloft/internal/player"
)

// TestPlaybackStatusMapsClientErrors 把"客户端传错"与"服务端故障"分开。
//
// 回归背景：成员列表为空、takeover_behavior 非法这类校验错误以前是裸 fmt.Errorf，
// playbackStatus 认不出来就一律 500，客户端把用户输入错误当成服务端故障。
func TestPlaybackStatusMapsClientErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"参数错误", player.ErrInvalidArgument, http.StatusBadRequest},
		{"参数错误（带文案）", fmt.Errorf("%w", &player.ArgumentError{Msg: "member_ids 不能为空"}), http.StatusBadRequest},
		{"源组没在播", player.ErrNothingPlaying, http.StatusConflict},
		{"设备不存在", player.ErrPlayerNotFound, http.StatusNotFound},
		{"解码槽位满", player.ErrBudgetExhausted, http.StatusServiceUnavailable},
		{"未知错误仍是 500", fmt.Errorf("boom"), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := playbackStatus(tt.err); got != tt.want {
				t.Fatalf("playbackStatus(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

// TestArgumentErrorKeepsMessage 文案要原样给用户，不能变成哨兵错误里的通用文案。
func TestArgumentErrorKeepsMessage(t *testing.T) {
	err := &player.ArgumentError{Msg: "group_ids 不能为空"}
	if err.Error() != "group_ids 不能为空" {
		t.Fatalf("Error() = %q", err.Error())
	}
}
