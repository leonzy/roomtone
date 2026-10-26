package player_test

import (
	"context"
	"testing"
	"time"

	"songloft/internal/player"
)

// 播放生命周期不能继承触发它的请求 context。
//
// net/http 在 handler 返回时取消 r.Context()。引擎若直接继承它，起播协程
// 第一次循环就退出：线上表现为 POST /transport 返回 loading 后立刻
// state=error、error="start ffmpeg: context canceled"，整条服务端播放链路不可用。
func TestPlaybackOutlivesTriggerContext(t *testing.T) {
	m, builder, _ := newTestManager(t, "p1")
	e, err := m.Engine(m.GroupIDOf("p1"))
	if err != nil {
		t.Fatalf("engine: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	if err := e.Load(ctx, []int64{1}, 0, 0); err != nil {
		t.Fatalf("load: %v", err)
	}
	// 模拟 handler 返回：请求 context 立刻取消。
	cancel()

	s := builder.get("p1")
	waitFor(t, "起播后出帧", 2*time.Second, func() bool { return s.FrameCount() > 0 })

	before := s.FrameCount()
	time.Sleep(200 * time.Millisecond)
	if after := s.FrameCount(); after <= before {
		t.Fatalf("请求 context 取消后播放停了：frames %d → %d", before, after)
	}
	if st := e.Status(); st.State == player.StateError {
		t.Fatalf("请求 context 取消后进入 error：%s", st.Error)
	}
}
