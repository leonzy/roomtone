#!/usr/bin/env bash
# X-05：4 组并发解码的 CPU / 内存基线（对应 playback_development_plan.md X-05）
#
# 只依赖 ffmpeg/ffprobe，不需要真实音箱：按 internal/player/pipeline/decoder.go
# 的 decoderArgs 生成同参数（s16le 44.1k/2ch → pipe）的解码进程，4 路并发跑完
# 一首曲目，采样 CPU 与 RSS 峰值。
#
# 用法：
#   ./scripts/tests/decode_baseline.sh [曲目路径] [并发数]
#   ./scripts/tests/decode_baseline.sh testdata/sample.flac 4
# 未给曲目时用 ffmpeg 现场合成 3 分钟 44.1k/16bit 立体声正弦（临时目录，跑完删除）。
#
# 输出：每路耗时、总 CPU 时间、进程组 RSS 峰值、是否实时（耗时 <= 曲目时长）。
set -uo pipefail

TRACK="${1:-}"
N="${2:-4}"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

if ! command -v ffmpeg >/dev/null 2>&1; then
  echo "ffmpeg 未安装，跳过" >&2
  exit 2
fi

if [ -z "$TRACK" ]; then
  echo "生成 3 分钟测试音轨..."
  TRACK="$WORK/bench.wav"
  ffmpeg -nostdin -hide_banner -loglevel error -f lavfi \
    -i "sine=frequency=440:sample_rate=44100:duration=180" \
    -ac 2 -c:a pcm_s16le "$TRACK" || exit 1
fi

DUR="$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$TRACK")"
echo "曲目: $TRACK  时长: ${DUR}s  并发: $N"

start=$(date +%s.%N)
pids=()
for i in $(seq 1 "$N"); do
  # 与 decoderArgs 对齐：s16le / 44.1k / 2ch / pipe:1
  ffmpeg -nostdin -hide_banner -loglevel error -i "$TRACK" -vn \
    -f s16le -acodec pcm_s16le -ar 44100 -ac 2 pipe:1 \
    > /dev/null 2>"$WORK/err.$i" &
  pids+=($!)
done

peak_rss=0
while :; do
  alive=0
  rss=0
  for p in "${pids[@]}"; do
    if kill -0 "$p" 2>/dev/null; then
      alive=1
      r=$(ps -o rss= -p "$p" 2>/dev/null | tr -d ' ')
      [ -n "$r" ] && rss=$((rss + r))
    fi
  done
  [ "$rss" -gt "$peak_rss" ] && peak_rss=$rss
  [ "$alive" -eq 0 ] && break
  sleep 0.1
done

wait
end=$(date +%s.%N)
wall=$(echo "$end - $start" | bc)

echo "---- 基线 ----"
echo "wall:        ${wall}s（曲目 ${DUR}s，实时倍率 $(echo "scale=2; $DUR / $wall" | bc)x）"
echo "RSS 峰值:    $((peak_rss / 1024)) MiB（${N} 路合计）"
if [ "$(uname)" = "Darwin" ]; then
  echo "CPU:         $(ps -o %cpu= -p $$ | tr -d ' ')%（采样进程）"
else
  echo "CPU:         见下方 /usr/bin/time 输出"
fi
echo "结论判定：wall <= 曲目时长 且 RSS 峰值 < 512 MiB 视为通过"
