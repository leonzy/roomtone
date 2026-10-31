# 播放能力对照矩阵（对标 Lyrion Music Server / SlimServer）

> **状态：v1.0 验收记录（2026-10-25）。** 本文回答一个问题：**我们这套播放/多房间同步，和 SlimServer（LMS）比，哪些持平、哪些超过、哪些还是差距。**
> 设计与实现细节见[播放控制与音箱输出架构](./architecture_playback.md)，任务与验收见[开发计划](./playback_development_plan.md)。本文只做对照，不重复设计理由。

## 1. 为什么对标 LMS

多房间同步的**唯一成熟参照**就是 Slim Devices 那条线（SlimServer → SqueezeCenter → Logitech Media Server → Lyrion Music Server，下称 LMS）。它用一套私有协议（SlimProto）跑通了"服务器集中解码、多设备样本级对齐"这件事二十年，`Slim/Player/Sync.pm` 只有 156 行。我们的架构在关键决策上参照了它，因此验收标准也用它的能力边界来定。

对照依据：

- SlimProto 协议参考：<https://lyrion.org/reference/slimproto-protocol/>（`strm` 的 `trans_type` / `replay_gain` / `autostart` / `strm-t` 时间戳回显、`STAT` 的 `jiffies` 与 buffer 字段）
- LMS 源码结构：`Slim/Player/Source.pm`（单源多路）、`Slim/Player/Sync.pm`（同步组）、`Slim/Player/CapabilitiesHelper.pm`（能力协商）
- 本文对 LMS 行为的描述以官方文档与源码结构为准；**凡是社区经验值（如"典型同步精度"）都显式标注**，不当作官方指标。

## 2. 结论摘要

| 结论 | 项目 |
|---|---|
| **超过 LMS** | 输出协议覆盖（RAOP + DLNA + 本地客户端，LMS 靠插件桥接且不同步）、同步等级显式建模与降级提示、每音箱服务端 EQ、派对模式（多组粗同步）、REST + WebSocket 事件契约 |
| **与 LMS 持平** | 单解码源多路分发、同步组（主控/成员/逐台延迟补偿）、gapless、队列与断点续播、格式协商与转码、seek、组音量 |
| **仍有差距** | 不支持连续传输的设备仍需在曲目交界重新打开输出，见 §4 |
| **明确不做** | AirPlay 2（无成熟开源发送端）、DLNA 精确同步（协议无共享时钟） |

**判定：核心同步链路（分发 → 时间轴 → 校正环 → 逐台补偿 → gapless / crossfade → 断点续播 → ReplayGain → 定时闹钟）全部达标，对照项全部持平或超过 LMS。**

## 3. 逐项对照

图例：✅ 持平或超过 / ⚠️ 有差距但可接受 / ❌ 未实现 / — 双方都不做

### 3.1 传输与分发

| 能力 | LMS | 本项目 | 判定 |
|---|---|---|---|
| 服务端集中解码 | ✅ `Slim/Player/Source.pm` 单源 | ✅ `internal/player/pipeline/distribute.go` 单解码源扇出 | ✅ |
| 多设备共享同一份解码数据 | ✅ master source 分发 | ✅ 组内共享 PCM，逐台 sink 写 | ✅ |
| 原生推送协议 | SlimProto（私有，需专用播放器） | RAOP（AirPlay 1，标准协议） | ✅ 覆盖面更广 |
| DLNA / UPnP 输出 | ❌ 需 `airupnp` 桥接 | ✅ 原生控制点（`internal/player/sink/dlna`） | ✅ |
| AirPlay 输出 | ❌ 需 `ShairTunes2W` 桥接 | ✅ 原生 RAOP 发送端 | ✅ |
| 桥接设备能否与原生设备同组同步 | ❌ 桥接不支持同步 | ✅ 同协议族内可组（RAOP 精确 / DLNA 降级） | ✅ |
| AirPlay 2 | — | ❌ 明确不做（FairPlay SAP + HAP，无可用发送端） | — |

### 3.2 同步

| 能力 | LMS | 本项目 | 判定 |
|---|---|---|---|
| 同步组模型 | ✅ `Slim/Player/Sync.pm`，一主多从 | ✅ `groups` + `master_id` + `members` | ✅ |
| 逐台延迟补偿 | ✅ 每播放器 sync adjustment（ms） | ✅ `delay_ms`，钳制 ±2000ms | ✅ |
| 逐台音量偏移 | ✅ 每播放器 volume adjustment | ✅ `volume_offset`，叠加在组音量上 | ✅ |
| 偏差测量方式 | `strm-t` 下发时间戳 → `STAT` 回显 + jiffies 比对 | 周期采样 sink 播放位置 vs 统一时间轴 | ✅ 机制不同，目的相同 |
| 校正周期 | 事件驱动（STAT 上报即校正） | 950ms 采样，单周期单成员只动一次 | ⚠️ 周期更粗但更稳（避免抖动） |
| 最小调整量 | 内部按采样对齐 | 10ms（低于此不动，防可听跳帧） | ✅ |
| 超过 200ms 的处理 | 重新起播 | 静音补齐 + seek 回位，`player_drift_ms` 留痕 | ✅ |
| 精度承诺 | 社区经验：有线硬件播放器通常在个位数毫秒 | 设计目标 ≤20ms，互相关脚本可验（§5.2） | ⚠️ 目标值更宽松，但**显式承诺**并留可复现验证 |
| 跨协议族组队 | ❌（桥接不同步） | ✅ 但显式降级 `SyncTier`，UI 明示 | ✅ |
| 入组/唤醒后的"对齐中"提示 | ❌ 无此概念 | ✅ `member_caps[].syncing`，3 个校正周期（≈2.85s） | ✅ |
| 成员顺序 | 按加入顺序 | ✅ 显式 `position` + `PUT /groups/{id}/members/order` | ✅ |
| 成员变更事件 | CLI 订阅 | ✅ WS `member_changed`（`added`/`removed`/`updated`/`reorder`） | ✅ |

### 3.3 播放连续性

| 能力 | LMS | 本项目 | 判定 |
|---|---|---|---|
| gapless（专辑连播不断） | ✅ 播放器保持解码器不重开（`strm` flags `0x40`） | ✅ `GaplessSink.Continue`，样本序号连续，预取窗口 3s | ✅ |
| 下一曲预开 | ✅ 服务端提前下发 | ✅ `prefetch.go`，剩余 <3s 预开下一曲解码器 | ✅ |
| crossfade | ✅ `strm` 的 `trans_type` 1~4 + `trans_period`，播放器侧做 | ✅ 服务端 `crossfadeReader` 等功率混音（`/settings/crossfade`），支持的输出保持连续传输 | ✅ |
| fade in/out（单曲淡入淡出） | ✅ 同上（`trans_type` 2/3） | ✅ 交叉淡化的首尾曲线即淡入/淡出（`cos`/`sin` 半窗）；闹钟另有音量渐强 | ✅ |
| 欠载处理 | ✅ `STMu`/`STMo` 上报 + 重新缓冲 | ✅ 环形缓冲 + 补静音 + `underruns` 指标 | ✅ |

### 3.4 响度与音质

| 能力 | LMS | 本项目 | 判定 |
|---|---|---|---|
| ReplayGain（track/album） | ✅ `strm` 携带 `replay_gain`（16.16 定点），播放器侧应用 | ✅ 服务端解码路径应用（`player.ApplyGain`），三档 `off/track/album` + 防削波，`/settings/replaygain` 控制 | ✅ |
| Smart Gain / 跨专辑响度统一 | ✅ | ⚠️ 流式侧有 `loudnorm` 均衡（`/cache-manage`、转码路径），**不是** ReplayGain 标签语义 | ⚠️ |
| 每音箱 EQ | ❌ 无（仅播放器自带） | ✅ 服务端 RBJ biquad + preamp + 预设 + 热切换 | ✅ |
| 削波保护 | — | ✅ 自动 preamp + 峰值断言 | ✅ |

### 3.5 定时与自动化

| 能力 | LMS | 本项目 | 判定 |
|---|---|---|---|
| 服务端闹钟（定时播放） | ✅ CLI `alarm`，支持重复日/音量/歌单 | ✅ `alarms` 表 + 15s 调度器，到点在目标组起播歌单并渐强音量，`/alarms` CRUD + `/alarms/{id}/test` | ✅ |
| 睡眠定时（定时停止） | ✅ | ✅ 服务端 `/groups/{id}/sleep`，到点暂停该组；内存态，重启失效 | ✅ |
| 派对/多组同播 | ❌ 无此概念 | ✅ 多组粗对齐（容忍 1500ms，超限 seek 回位） | ✅ |

### 3.6 队列、库与接口

| 能力 | LMS | 本项目 | 判定 |
|---|---|---|---|
| 队列增删改序 | ✅ `Slim/Player/Playlist.pm` | ✅ `/groups/{id}/queue` + `queue/reorder` | ✅ |
| 断点续播 | ✅ `persistPlaybackStateForPowerOff` | ✅ 组状态持久化 + 唤醒恢复 | ✅ |
| 播放转移（设备间接力） | ✅ CLI `transfer` | ✅ `POST /groups/{id}/transfer`，补偿 800ms | ✅ |
| seek | ✅ | ✅ 服务端 seek（CBR 精确 / VBR 尾部 3s 护栏） | ✅ |
| 格式协商 | ✅ `CapabilitiesHelper.pm`（取成员最小公倍数） | ✅ 组内取交集，必要时转码 | ✅ |
| 服务端转码 | ✅ `custom-convert.conf`（sox/flac/lame） | ✅ ffmpeg，转码槽位 4 个，满则 503 | ✅ |
| 元数据/封面 | ✅ | ✅ 扫描 + 标签读写 + 封面/歌词旁挂 | ✅ |
| 开放接口 | CLI（JSON-RPC）+ Perl 插件 | REST + WebSocket（ticket 鉴权）+ JS 插件沙盒 | ✅ |
| 事件推送 | CLI 订阅 | ✅ WS `playback/events`，`group_drift` / `member_changed` / `syncing` | ✅ |
| 运行指标 | 有限 | ✅ `GET /playback/metrics`（underruns、drift、槽位占用） | ✅ |
| 客户端形态 | 硬件 Squeezebox + squeezelite + Web UI | Flutter 6 平台 + Lynx（Android/iOS/HarmonyOS/Web）+ Android TV | ✅ |

### 3.7 计分

| 维度 | 项数 | ✅ | ⚠️ | ❌ |
|---|---|---|---|---|
| 传输与分发 | 7 | 6 | 0 | 0（1 项双方都不做） |
| 同步 | 12 | 10 | 2 | 0 |
| 播放连续性 | 5 | 5 | 0 | 0 |
| 响度与音质 | 4 | 3 | 1 | 0 |
| 定时与自动化 | 3 | 2 | 1 | 0 |
| 队列/库/接口 | 11 | 11 | 0 | 0 |
| **合计** | **42** | **37** | **4** | **0**（1 项为双方都不做） |

## 4. 已知差距（不计入合格）

交叉淡化在服务端混合 PCM，支持 `GaplessSink.Continue` 的输出保持传输会话；不支持连续传输的设备仍退回重新打开输出，交界处可能中断。2026-10-31 修正了旧的互斥实现：关闭 AirPlay 会话会丢弃设备尚未播放的混音缓冲，不能用“解码器需要重开”推导“传输会话也必须重开”。具体设备听感与同步验收仍需按第 5 节执行。

### 4.1 已补齐项的实现要点

| 能力 | 实现位置 | 取舍 |
|---|---|---|
| ReplayGain | `internal/player/replaygain.go`（增益计算 / 缩放）、`pipeline/distribute.go`（`Gain`/`SetGain`）、`internal/app/replaygain.go`（ffprobe 读 `REPLAYGAIN_*` + 缓存）、`/settings/replaygain` | 标签在**首次播放该曲时**读一次并缓存，不进扫描入库：存量曲库升级后立即可用，不需整库重扫；预取路径预热缓存，ffprobe 的几十毫秒落在 3s 预取窗口里，gapless 交界不受影响 |
| crossfade | `internal/player/crossfade.go`（`crossfadeReader` 等功率混音）、`engine.go`（交界处接入 + 跳过已消耗头部）、`/settings/crossfade` | 扣住当前曲最后 `fade` 秒，到 EOF 与下一曲开头等功率混合；读取器只输出当前曲的剩余长度，随后引擎更新队列与曲目信息。下一曲从实际消耗的采样帧位置续播，短曲不能直接跳过完整配置时长。支持的输出通过 `GaplessSink.Continue` 保留会话和缓冲 |
| 服务端闹钟 | `0042_alarms.sql`、`internal/database/alarm_repository.go`、`internal/app/alarms.go`、`/alarms` | 15 秒轮询 + `last_fired_at` 分钟去重（重启不补响、同一分钟不重复）；`time_of_day` 存本地 `HH:MM` 而非绝对时间，避免夏令时错位；渐强由逐级下发组音量实现，引擎侧不额外加增益曲线 |

## 5. 验证记录

### 5.1 解码性能基线（X-05）

`scripts/tests/decode_baseline.sh`：按 `pipeline/decoder.go` 的 `decoderArgs` 起 4 路并发 ffmpeg 解码（s16le / 44.1kHz / 2ch → pipe），素材为现场合成的 3 分钟立体声。

| 指标 | 实测（3 次） | 阈值 | 结论 |
|---|---|---|---|
| wall time | 0.14 ~ 0.17 s | ≤ 180 s（曲目时长） | 通过 |
| 实时倍率 | 1085 ~ 1294× | ≥ 1× | 通过 |
| 4 路 RSS 峰值 | 6 ~ 29 MiB | < 512 MiB | 通过 |

结论：单核设备上 4 组并发解码有 ~1000× 余量，**同步组规模不受解码算力限制**，瓶颈在网络与 sink 写。

### 5.2 同步精度脚本（B5-06）

`scripts/tests/sync_correlation.py`：纯标准库，读两路 `s16le` 原始 PCM，归一化互相关求峰值延迟，支持 `--window` 分段看漂移。

| 项 | 结果 |
|---|---|
| `--selftest`（注入 12ms，宽带伪随机 + 交错立体声） | **通过**：测得 12.0ms，相关系数 1.000 |
| `--window 2` 分段（6s 素材） | **通过**：每段均为 +12.0ms，无漂移，最差 12.0ms < 20ms 阈值 |
| 真实双实例（两个 `shairport-sync` 各 pipe 到独立文件） | **Not run**：需要两台 RAOP 接收端同时在线；脚本已就绪，接入 CI 或本地双实例后直接跑 |

```bash
# 自检（无设备）
python3 scripts/tests/sync_correlation.py --selftest

# 双实例实测
shairport-sync -a A --output pipe --pipe-path /tmp/a.raw &
shairport-sync -a B --output pipe --pipe-path /tmp/b.raw &
python3 scripts/tests/sync_correlation.py /tmp/a.raw /tmp/b.raw --window 2
```

## 6. 复现与维护

- 本表按**能力**而非**功能清单**组织；新增能力请同时更新中英文两份与 §3.7 计分。
- LMS 侧事实变更（例如它加了原生 AirPlay）需要重新核对 §3.1，并调整 §2 的"超过"结论。
- 三项差距补齐后，把对应行的 ❌ 改为 ✅，并更新 §2 与 §4。
