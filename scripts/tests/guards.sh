#!/usr/bin/env bash
# 播放子系统半成品守卫（X-07）。
#
# 为什么需要它：这些约束单看代码看不出来，而一旦被"顺手改回去"，
# 现象是"声音有点不对"而不是"测试红了"。所以用 rg 把它们钉成红线：
#   1. EQ 只能在服务端逐样本做（dsp 包），不许回到 ffmpeg 滤镜链；
#   2. 起播水位判据必须严格小于分发器推送深度（否则永远卡 buffering）；
#   3. 校正环、派对回位、生命周期端点的用例必须存在；
#   4. 新端点必须在 swagger 里能查到。
#
# 用法：make check-playback（CI 里由 make check 串起来）
set -uo pipefail
cd "$(dirname "$0")/../.." || exit 1

fail=0
note() { printf '  %s\n' "$1"; }
ok() { printf '\033[32m✓\033[0m %s\n' "$1"; }
bad() { printf '\033[31m✗\033[0m %s\n' "$1"; fail=1; }

echo "== 守卫 1：EQ 不得回到 ffmpeg 滤镜链 =="
# 只看播放子系统：services 里的 -af 是响度均衡/转码用的，与音箱 EQ 无关。
if rg -n --glob '!*_test.go' -e 'BuildFilterChain' -e 'equalizer=f=' -e '"-af"' internal/player/ | grep -q .; then
	rg -n --glob '!*_test.go' -e 'BuildFilterChain' -e 'equalizer=f=' -e '"-af"' internal/player/
	bad "发现 ffmpeg 滤镜式 EQ（EQ 必须走 internal/player/dsp 逐样本处理）"
else
	ok "无 ffmpeg 滤镜式 EQ"
fi

echo "== 守卫 2：起播水位必须小于分发推送深度 =="
python3 - <<'PY' || fail=1
import re, sys

UNIT = {"Nanosecond": 1e-6, "Microsecond": 1e-3, "Millisecond": 1.0,
        "Second": 1000.0, "Minute": 60000.0}

def duration_ms(expr):
    """把 '1 * time.Second' / 'time.Second' / '950 * time.Millisecond' 换算成毫秒。"""
    m = re.fullmatch(r'(?:([0-9.]+)\s*\*\s*)?time\.([A-Za-z]+)', expr.strip())
    if not m:
        return None
    n = float(m.group(1) or 1)
    unit = UNIT.get(m.group(2))
    return None if unit is None else n * unit

def const_ms(path, name):
    src = open(path, encoding="utf-8").read()
    m = re.search(r'^(?:const\s+)?' + name + r'\s*=\s*(.+)$', src, re.M)
    return duration_ms(m.group(1)) if m else None

wm = const_ms("internal/player/transport.go", "DefaultBufferWatermark")
depth = const_ms("internal/player/pipeline/distribute.go", "DefaultBufferDepth")
if wm is None or depth is None:
    print("  ✗ 找不到水位或推送深度常量（守卫失效，需同步更新）")
    sys.exit(1)
if wm >= depth:
    print(f"  ✗ 起播水位 {wm:g}ms ≥ 推送深度 {depth:g}ms：水位永远顶不到，播放会卡在 buffering")
    sys.exit(1)
print(f"  ✓ 起播水位 {wm:g}ms < 推送深度 {depth:g}ms")
PY

echo "== 守卫 3：关键用例必须存在 =="
for f in internal/player/party_test.go internal/player/lifecycle_test.go internal/player/metrics_test.go internal/player/sync_loop_test.go; do
	if [ -f "$f" ]; then ok "存在 $f"; else bad "缺少 $f"; fi
done

echo "== 守卫 4：新端点必须进 swagger =="
if [ ! -f docs/swagger.json ]; then
	bad "docs/swagger.json 不存在（先跑 make swagger）"
else
	for p in '/players/{id}/power' '/players/{id}/eq/presets' '/playback/metrics' '/groups/{id}/master' '/groups/{id}/members/order' '/groups/{id}/sleep' '/settings/replaygain' '/settings/crossfade' '/alarms' '/alarms/{id}/test'; do
		if grep -q "\"$p\"" docs/swagger.json; then ok "swagger 含 $p"; else bad "swagger 缺 $p"; fi
	done
fi

echo "== 守卫 5：组准入不得静默降级 =="
if rg -n 'ErrSyncTierMismatch' internal/player/manager.go > /dev/null; then
	ok "精确组准入有显式拒绝"
else
	bad "精确同步组准入的显式拒绝被删掉了"
fi

if [ "$fail" -ne 0 ]; then
	echo "守卫未通过"
	exit 1
fi
echo "全部守卫通过"
