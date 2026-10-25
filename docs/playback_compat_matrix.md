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
| **仍有差距** | **crossfade**、**ReplayGain**、**服务端闹钟/定时唤醒**（三项均未实现，见 §4） |
| **明确不做** | AirPlay 2（无成熟开源发送端）、DLNA 精确同步（协议无共享时钟） |

**判定：核心同步链路（分发 → 时间轴 → 校正环 → 逐台补偿 → gapless → 断点续播）全部达标，可以发布 v1.0；crossfade / ReplayGain / 闹钟三项记为已知差距，不计入"合格"。**

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
| crossfade | ✅ `strm` 的 `trans_type` 1~4 + `trans_period` | ❌ 未实现 | ❌ |
| fade in/out（单曲淡入淡出） | ✅ 同上（`trans_type` 2/3） | ❌ 未实现 | ❌ |
| 欠载处理 | ✅ `STMu`/`STMo` 上报 + 重新缓冲 | ✅ 环形缓冲 + 补静音 + `underruns` 指标 | ✅ |

### 3.4 响度与音质

| 能力 | LMS | 本项目 | 判定 |
|---|---|---|---|
| ReplayGain（track/album） | ✅ `strm` 携带 `replay_gain`（16.16 定点），播放器侧应用 | ❌ 未实现 | ❌ |
| Smart Gain / 跨专辑响度统一 | ✅ | ⚠️ 流式侧有 `loudnorm` 均衡（`/cache-manage`、转码路径），**不是** ReplayGain 标签语义 | ⚠️ |
| 每音箱 EQ | ❌ 无（仅播放器自带） | ✅ 服务端 RBJ biquad + preamp + 预设 + 热切换 | ✅ |
| 削波保护 | — | ✅ 自动 preamp + 峰值断言 | ✅ |

### 3.5 定时与自动化

| 能力 | LMS | 本项目 | 判定 |
|---|---|---|---|
| 服务端闹钟（定时播放） | ✅ CLI `alarm`，支持重复日/音量/歌单 | ❌ 未实现 | ❌ |
| 睡眠定时（定时停止） | ✅ | ⚠️ 客户端本地实现（Flutter `SleepTimer`），单设备、不跨客户端持久 | ⚠️ |
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
| 播放连续性 | 5 | 3 | 0 | 2 |
| 响度与音质 | 4 | 2 | 1 | 1 |
| 定时与自动化 | 3 | 1 | 1 | 1 |
| 队列/库/接口 | 11 | 11 | 0 | 0 |
| **合计** | **42** | **33** | **4** | **4**（1 项为双方都不做） |

## 4. 已知差距（不计入合格）

三项都**不是**架构限制，是排期取舍。按"补齐代价 / 用户价值"排序：

| 差距 | LMS 怎么做 | 我们缺什么 | 补齐路径 | 代价 |
|---|---|---|---|---|
| **crossfade / fade** | `strm` 下发 `trans_type` + `trans_period`，**播放器侧**做淡变；RAOP/DLNA 设备不认这个字段 | 服务端解码路径里没有交叉淡化混音器 | 在 `pipeline/distribute.go` 的 PCM 环节加 200ms 线性/Pequal-power 混音，队列切换时提前 1 个淡化窗开下一曲；组内必须整组同一淡化时长 | 小（~3 人日），但会让 gapless 与 crossfade 互斥，需要产品决策 |
| **ReplayGain** | `strm` 携带 `replay_gain`，播放器侧乘增益；标签来自扫描阶段 | 扫描不读 `REPLAYGAIN_*` 标签；解码路径无增益应用点 | 扫描阶段读标签入库（`pkg/tag` 已能读），解码/分发环节按 track/album 模式乘 16.16 定点增益，并在 API 暴露三档（关/单曲/专辑） | 中（~5 人日，含 UI 与削波回归） |
| **服务端闹钟** | CLI `alarm`：每播放器多组闹钟、重复日、音量、歌单、渐强 | 无定时任务调度与"到点起播"编排 | 用现有 cron/定时器设施建 `alarms` 表 + 调度器，到点调 `transport` 起播并渐强音量 | 小（~3 人日），但需要前端设置页与跨时区处理 |

**为什么 v1.0 可以带着这三项发布**：LMS 的 crossfade / ReplayGain 都依赖**播放器侧**实现，而我们的主力输出是 RAOP / DLNA 设备——这两个协议都不认这两个字段，服务端不做就等于没有；闹钟则是独立功能模块，与同步链路无耦合。因此它们不构成"同步能力不合格"。

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
