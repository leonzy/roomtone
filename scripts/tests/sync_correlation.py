#!/usr/bin/env python3
"""B5-06：双设备同步精度测量（互相关求峰值延迟）

两个 shairport-sync 实例各把 PCM 管道到独立文件，同时播放同一段素材，
用互相关求两路之间的偏移（采样级），断言 < 20ms；重复采样可看是否漂移。

用法：
  # 1) 采集（两台机器 / 两个实例，同一段素材同时播放）
  #    shairport-sync -c a.conf   →  pipe →  a.raw
  #    shairport-sync -c b.conf   →  pipe →  b.raw
  # 2) 分析
  ./sync_correlation.py a.raw b.raw --rate 44100 --channels 2 --max-offset-ms 20
  # 分段看漂移（每 30s 一个窗口）
  ./sync_correlation.py a.raw b.raw --window 30
  # 无素材时自检：合成两路（第二路延迟 12ms）
  ./sync_correlation.py --selftest

raw 文件格式：有符号 16bit 小端交错 PCM（shairport-sync 的 pipe 默认输出）。
纯标准库实现（array + 手写互相关），不依赖 numpy。
"""
from __future__ import annotations

import argparse
import array
import math
import sys


def read_pcm(path: str, channels: int) -> list[int]:
    a = array.array("h")
    with open(path, "rb") as f:
        a.frombytes(f.read())
    if sys.byteorder == "big":
        a.byteswap()
    if channels > 1:
        # 取单声道（各声道求和），避免左右差异干扰
        mono = [0] * (len(a) // channels)
        for i in range(len(mono)):
            base = i * channels
            mono[i] = sum(a[base:base + channels]) // channels
        return mono
    return list(a)


def best_offset_ms(ref: list[int], test: list[int], rate: int, max_lag: int) -> tuple[float, float]:
    """在 ±max_lag 采样内做互相关，返回 (偏移毫秒, 归一化相关系数)。

    正值表示 test 比 ref 晚（test 落后）。
    """
    n = min(len(ref), len(test))
    if n <= max_lag * 4:
        raise SystemExit(f"样本太少：{n} 点，至少需要 {max_lag * 4}")
    # 取中段，避免两端静音
    seg = min(n // 2, rate * 5)
    r0 = (n - seg) // 2
    ref_seg = ref[r0:r0 + seg]

    best_lag, best_corr = 0, -2.0
    for lag in range(-max_lag, max_lag + 1):
        t0 = r0 + lag
        if t0 < 0 or t0 + seg > len(test):
            continue
        corr = 0.0
        ea = 0.0
        eb = 0.0
        for i in range(0, seg, 4):  # 4 倍抽取；精度由窄带细搜补回
            x = ref_seg[i]
            y = test[t0 + i]
            corr += x * y
            ea += x * x
            eb += y * y
        # 归一化相关系数，避免幅度大的位置胜出
        norm = corr / math.sqrt(ea * eb + 1e-9)
        if norm > best_corr:
            best_lag, best_corr = lag, norm
    return best_lag * 1000.0 / rate, best_corr


def selftest() -> int:
    rate, ch, delay_ms = 44100, 2, 12
    total = rate * 10
    # 宽带伪随机噪声（LCG 固定种子）：互相关单峰，避免周期信号的歧义峰
    state = 12345
    src = []
    for _ in range(total):
        state = (1103515245 * state + 12345) & 0x7FFFFFFF
        src.append(int((state / 0x7FFFFFFF - 0.5) * 20000))
    d = int(rate * delay_ms / 1000)
    delayed = [0] * d + src[:-d]
    # 交错立体声（每样本重复两次），与 read_pcm(channels=2) 的取单声道口径一致
    a = array.array("h", [v for v in src for _ in range(ch)])
    b = array.array("h", [v for v in delayed for _ in range(ch)])
    for x in (a, b):
        if sys.byteorder == "big":
            x.byteswap()
    with open("/tmp/_sync_a.raw", "wb") as f:
        a.tofile(f)
    with open("/tmp/_sync_b.raw", "wb") as f:
        b.tofile(f)
    off, corr = best_offset_ms(read_pcm("/tmp/_sync_a.raw", ch), read_pcm("/tmp/_sync_b.raw", ch), rate, int(rate * 0.2))
    print(f"自检：注入 {delay_ms}ms，测得 {off:.1f}ms，相关系数 {corr:.3f}")
    ok = abs(off - delay_ms) < 1.0
    print("自检通过" if ok else "自检失败")
    return 0 if ok else 1


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("ref", nargs="?")
    ap.add_argument("test", nargs="?")
    ap.add_argument("--rate", type=int, default=44100)
    ap.add_argument("--channels", type=int, default=2)
    ap.add_argument("--max-offset-ms", type=float, default=20.0)
    ap.add_argument("--window", type=float, default=0, help="分段窗口秒数，用于观察漂移")
    ap.add_argument("--selftest", action="store_true")
    args = ap.parse_args()

    if args.selftest:
        return selftest()
    if not (args.ref and args.test):
        ap.error("需要两个 raw 文件，或用 --selftest")

    ref = read_pcm(args.ref, args.channels)
    test = read_pcm(args.test, args.channels)
    max_lag = int(args.rate * 0.2)  # ±200ms 搜索范围

    if args.window <= 0:
        off, corr = best_offset_ms(ref, test, args.rate, max_lag)
        print(f"偏移 {off:+.1f}ms（相关系数 {corr:.3f}）")
        ok = abs(off) <= args.max_offset_ms
        print(f"阈值 {args.max_offset_ms}ms → {'通过' if ok else '不通过'}")
        return 0 if ok else 1

    win = int(args.window * args.rate)
    worst = 0.0
    for start in range(0, min(len(ref), len(test)) - win, win):
        off, corr = best_offset_ms(ref[start:start + win], test[start:start + win], args.rate, max_lag)
        worst = max(worst, abs(off))
        print(f"t={start / args.rate:7.1f}s  偏移 {off:+7.1f}ms  相关系数 {corr:.3f}")
    ok = worst <= args.max_offset_ms
    print(f"最差偏移 {worst:.1f}ms，阈值 {args.max_offset_ms}ms → {'通过（无漂移）' if ok else '不通过'}")
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
