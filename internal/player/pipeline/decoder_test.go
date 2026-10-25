package pipeline

import (
	"strings"
	"testing"
	"time"

	"songloft/internal/player"
	"songloft/internal/player/sink"
)

// B6-07 断言：解码器命令行里不再出现任何滤镜。
//
// EQ 已改为分发之后逐样本处理，ffmpeg 只负责解码。这条断言防止有人
// 图省事把 equalizer 滤镜加回来——那会让每台音箱各起一个解码源。
func TestDecoderArgsCarryNoFilters(t *testing.T) {
	args := decoderArgs(player.Track{Path: "/music/a.flac"}, 12*time.Second,
		sink.Format{Codec: player.CodecL16, SampleRate: 44100, Channels: 2, Bits: 16})
	joined := strings.Join(args, " ")
	for _, bad := range []string{"-af", "equalizer=f=", "volume="} {
		if strings.Contains(joined, bad) {
			t.Fatalf("ffmpeg 命令行不应含 %q：%s", bad, joined)
		}
	}
	if !strings.Contains(joined, "-ss 12.000") {
		t.Fatalf("起播位置丢了：%s", joined)
	}
	if !strings.Contains(joined, "-ar 44100") || !strings.Contains(joined, "-ac 2") {
		t.Fatalf("输出格式不对：%s", joined)
	}
	if args[len(args)-1] != "pipe:1" {
		t.Fatalf("必须输出到 stdout：%s", joined)
	}
}
